package admin

import (
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"wpqu-panel/internal/signing"
)

// keyState 描述一个私钥的状态与自检结果。
type keyState struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Public   string `json:"public"`
	Expected string `json:"expected"`
	Match    bool   `json:"match"`
}

// handleOverview 汇总一屏能看完的状态。
//
// 这里最重要的一项不是「有多少插件」，而是四个公钥是否成对：
// 私钥的公钥部分必须与编译进面板的常量一致，否则签出来的包和码
// 客户那边一律验不过，而界面上看起来一切正常。
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	keys := []keyState{
		checkKey("发布签名", s.cfg.keyPath("release"), signing.ReleasePublicKey),
		checkKey("授权签发", s.cfg.keyPath("license"), signing.LicensePublicKey),
	}

	var warnings []string
	for _, k := range keys {
		if !k.Match {
			warnings = append(warnings, k.Name+"私钥与面板内置公钥不匹配：用它签出的东西客户那边验不过")
		}
	}

	plugins, _ := s.listPlugins()
	published, drift := 0, 0
	for _, p := range plugins {
		if p.Published.Exists {
			published++
		}
		if p.Drift != "" {
			drift++
		}
	}
	if drift > 0 {
		warnings = append(warnings,
			strconv.Itoa(drift)+" 个插件改动还没发布，客户现在拿到的是旧包")
	}

	now := time.Now()
	summary := map[string]int{"total": 0, "active": 0, "expiring": 0, "expired": 0}
	for _, e := range s.ledger.All() {
		summary["total"]++
		switch statusOf(e, now) {
		case "已过期":
			summary["expired"]++
		case "即将到期":
			summary["expiring"]++
		case "已作废":
			// 作废不计入活跃
		default:
			summary["active"]++
		}
	}
	if summary["expiring"] > 0 {
		warnings = append(warnings, strconv.Itoa(summary["expiring"])+" 张授权码 30 天内到期")
	}

	meta, releases := s.readUpdates()
	if len(releases) == 0 {
		warnings = append(warnings, "还没有发布过升级清单，面板的「检查更新」会失败")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"admin": map[string]any{
			"version":   Version,
			"startedAt": s.started.Format(time.RFC3339),
			"uptime":    time.Since(s.started).Round(time.Second).String(),
		},
		"keys":     keys,
		"plugins":  map[string]any{"total": len(plugins), "published": published, "drift": drift},
		"licenses": summary,
		"updates": map[string]any{
			"latest":       meta["latest"],
			"minSupported": meta["minSupported"],
			"releases":     len(releases),
		},
		"paths": map[string]any{
			"site": s.cfg.SiteDir,
			"src":  s.cfg.SrcDir,
			"dist": s.cfg.DistDir,
			"keys": s.cfg.KeysDir,
			"data": s.cfg.DataDir,
		},
		"warnings": warnings,
	})
}

func checkKey(name, path, expected string) keyState {
	st := keyState{Name: name, Path: path, Expected: expected}
	if !fileExists(path) {
		return st
	}
	st.Exists = true
	pub, err := signing.PublicKeyOf(path)
	if err != nil {
		return st
	}
	st.Public = pub
	st.Match = pub == expected
	return st
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": s.audit.Read(limit)})
}

// SiteFile 是给测试用的路径访问器（避免把 cfg 字段散出去）。
func (s *Server) SiteFile(rel string) string {
	return filepath.Join(s.cfg.SiteDir, rel)
}
