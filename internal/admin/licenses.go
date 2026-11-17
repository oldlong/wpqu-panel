package admin

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"wpqu-panel/internal/licensing"
	"wpqu-panel/internal/signing"
)

// routeLicenses 处理 /api/licenses 下的全部接口。
func (s *Server) routeLicenses(w http.ResponseWriter, r *http.Request, parts []string) {
	id, action := "", ""
	if len(parts) > 2 {
		id = parts[2]
	}
	if len(parts) > 3 {
		action = parts[3]
	}

	switch {
	case id == "" && r.Method == http.MethodGet:
		s.handleLicenseList(w, r)
	case id == "export" && r.Method == http.MethodGet:
		s.handleLicenseExport(w, r)
	case id == "verify" && r.Method == http.MethodPost:
		s.handleLicenseVerify(w, r)
	case id == "" && r.Method == http.MethodPost:
		s.handleLicenseIssue(w, r)
	case id != "" && action == "void" && r.Method == http.MethodPost:
		s.handleLicenseVoid(w, r, id)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的操作"})
	}
}

// licenseRow 是台账一行的界面形态：台账字段 + 实时状态。
type licenseRow struct {
	Entry
	Status string `json:"status"`
	Days   int    `json:"days"`
}

func (s *Server) handleLicenseList(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	product := r.URL.Query().Get("product")
	status := r.URL.Query().Get("status")

	now := time.Now()
	all := s.ledger.All()
	rows := make([]licenseRow, 0, len(all))
	summary := map[string]int{"total": 0, "active": 0, "expiring": 0, "expired": 0, "void": 0}

	for _, e := range all {
		st := statusOf(e, now)
		lic := licensing.License{Product: e.Product, Expire: e.Expire}
		row := licenseRow{Entry: e, Status: st, Days: lic.DaysLeft(now)}

		summary["total"]++
		switch st {
		case "已作废":
			summary["void"]++
		case "已过期":
			summary["expired"]++
		case "即将到期":
			summary["expiring"]++
		default:
			summary["active"]++
		}

		if q != "" && !strings.Contains(strings.ToLower(e.Name+" "+e.Product+" "+e.Note+" "+e.ID), q) {
			continue
		}
		if product != "" && e.Product != product {
			continue
		}
		if status != "" && st != status {
			continue
		}
		rows = append(rows, row)
	}
	// 新的在前
	sort.Slice(rows, func(i, j int) bool { return rows[i].IssuedAt > rows[j].IssuedAt })

	writeJSON(w, http.StatusOK, map[string]any{
		"licenses": rows,
		"summary":  summary,
		"products": s.licenseProducts(),
	})
}

// licenseProducts 给发码表单的下拉框用：插件 id + 名称 + 是否收费。
func (s *Server) licenseProducts() []map[string]any {
	ids, err := s.pluginIDs()
	if err != nil {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		raw, err := os.ReadFile(filepath.Join(s.cfg.SrcDir, id, "plugin.json"))
		if err != nil {
			continue
		}
		mf := mustJSON(raw)
		out = append(out, map[string]any{
			"id":       id,
			"name":     mf["name"],
			"tier":     mf["tier"],
			"priceCny": intOf(mf["priceCny"]),
		})
	}
	// 收费的排前面：发码主要就是给收费插件发的，默认选中它少点一次
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := out[i]["tier"] == "paid", out[j]["tier"] == "paid"
		if pi != pj {
			return pi
		}
		return out[i]["id"].(string) < out[j]["id"].(string)
	})
	return out
}

func (s *Server) handleLicenseIssue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product string `json:"product"`
		Expire  string `json:"expire"`
		Seats   int    `json:"seats"`
		Name    string `json:"name"`
		Note    string `json:"note"`
		Count   int    `json:"count"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.Product = strings.TrimSpace(req.Product)
	req.Name = strings.TrimSpace(req.Name)
	if !validID(req.Product) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请选择插件"})
		return
	}
	if _, err := os.Stat(filepath.Join(s.cfg.SrcDir, req.Product, "plugin.json")); err != nil {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": fmt.Sprintf("没有 %s 这个插件（发出去的码会永远装不上）", req.Product)})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": "必须填客户名称：码里带着它，将来对账全靠这个"})
		return
	}
	if req.Count == 0 {
		req.Count = 1
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	codes, items, err := licensing.IssueMany(s.licenseKey, licensing.License{
		Product: req.Product,
		Expire:  req.Expire,
		Seats:   req.Seats,
		Name:    req.Name,
	}, req.Count)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	entries := make([]Entry, 0, len(items))
	for i, it := range items {
		e := Entry{
			ID:       it.ID,
			Code:     codes[i],
			Product:  it.Product,
			Expire:   it.Expire,
			Seats:    it.Seats,
			Name:     it.Name,
			Note:     req.Note,
			IssuedAt: it.Issued,
		}
		if err := s.ledger.Add(e); err != nil {
			// 码已经签出来了但没记上账：必须告诉用户，否则这张码成了黑户
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "码已签发但记账失败：" + err.Error(),
				"codes": codes,
			})
			return
		}
		entries = append(entries, e)
	}
	s.audit.Add(clientIP(r, s.cfg.BehindProxy), "license.issue",
		fmt.Sprintf("%s × %d", req.Product, len(entries)), req.Name+"｜"+req.Note)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "codes": codes, "entries": entries})
}

func (s *Server) handleLicenseVoid(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	entry, ok, err := s.ledger.Void(id, req.Reason)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "台账里没有这个 id"})
		return
	}
	s.audit.Add(clientIP(r, s.cfg.BehindProxy), "license.void", id, req.Reason)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"entry":  entry,
		"notice": "已标记作废。注意：这不会让码失效 —— 离线验签无法吊销，作废只用于台账管理。",
	})
}

// verifyResult 是「验一个码」的返回。
type verifyResult struct {
	OK       bool               `json:"ok"`
	Error    string             `json:"error,omitempty"`
	License  *licensing.License `json:"license,omitempty"`
	Decoded  *licensing.License `json:"decoded,omitempty"`
	Status   string             `json:"status,omitempty"`
	Product  map[string]any     `json:"product,omitempty"`
	InLedger bool               `json:"inLedger"`
	ExpireIn string             `json:"expireIn,omitempty"`
	// PanelKeyMatch 为假时，这个码虽然签名有效，但面板内置公钥与之不同，
	// 实际装插件时仍会被拒绝。
	PanelKeyMatch bool   `json:"panelKeyMatch"`
	Warning       string `json:"warning,omitempty"`
}

func (s *Server) handleLicenseVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	now := time.Now()
	res := verifyResult{}

	// 先试着解码：验签失败时也要能告诉用户「码里写的是谁」
	if dec, err := licensing.Decode(req.Code); err == nil {
		res.Decoded = dec
	}

	lic, err := licensing.ParseWith(s.licensePub, req.Code)
	if err != nil {
		res.Error = err.Error()
		writeJSON(w, http.StatusOK, res)
		return
	}
	res.OK = true
	res.PanelKeyMatch = s.licensePub == signing.LicensePublicKey
	if !res.PanelKeyMatch {
		res.Warning = "签名有效，但本机授权私钥对应的公钥与面板内置常量不一致，面板实际会拒绝这个码"
	}
	res.License = lic
	res.Status = statusOf(Entry{Product: lic.Product, Expire: lic.Expire, Seats: lic.Seats}, now)
	if d := lic.DaysLeft(now); d >= 0 {
		res.ExpireIn = fmt.Sprintf("%d 天", d)
	} else {
		res.ExpireIn = "永久"
	}
	if raw, err := os.ReadFile(filepath.Join(s.cfg.SrcDir, lic.Product, "plugin.json")); err == nil {
		mf := mustJSON(raw)
		res.Product = map[string]any{
			"id":       lic.Product,
			"name":     mf["name"],
			"version":  mf["version"],
			"tier":     mf["tier"],
			"priceCny": intOf(mf["priceCny"]),
		}
	}
	if _, ok := s.ledger.Get(lic.ID); ok {
		res.InLedger = true
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleLicenseExport(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	rows := s.ledger.All()
	sort.Slice(rows, func(i, j int) bool { return rows[i].IssuedAt < rows[j].IssuedAt })

	var buf bytes.Buffer
	buf.WriteString("\ufeff") // Excel 打开 UTF-8 CSV 需要 BOM，否则中文是乱码
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"id", "插件", "客户", "授权码", "到期日", "席位", "签发时间", "状态", "备注", "作废原因"})
	for _, e := range rows {
		_ = cw.Write([]string{
			e.ID, e.Product, e.Name, e.Code, e.Expire, strconv.Itoa(e.Seats),
			e.IssuedAt, statusOf(e, now), e.Note, e.VoidReason,
		})
	}
	cw.Flush()

	filename := "licenses-" + now.Format("20060102-1504") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
