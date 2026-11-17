package admin

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"wpqu-panel/internal/pack"
	"wpqu-panel/internal/version"
)

// publishedInfo 是线上正在分发的那个包。
type publishedInfo struct {
	Version string `json:"version"`
	Size    int    `json:"size"`
	SHA256  string `json:"sha256"`
	URL     string `json:"url"`
	Exists  bool   `json:"exists"`
}

// pluginView 是插件页一行所需的一切。
type pluginView struct {
	ID        string         `json:"id"`
	Manifest  map[string]any `json:"manifest"`
	Raw       string         `json:"raw,omitempty"`
	Published publishedInfo  `json:"published"`
	// Drift：unpublished 没发过 / version 版本号不同 / content 同版本但内容变了 / 空 表示一致
	Drift string     `json:"drift"`
	Files []fileInfo `json:"files,omitempty"`
}

type fileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// readIndex 读线上正在分发的市场索引。
func readIndex(siteDir string) map[string]map[string]any {
	raw, err := os.ReadFile(filepath.Join(siteDir, "api", "plugins", "index.json"))
	if err != nil {
		return nil
	}
	var idx struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil
	}
	out := make(map[string]map[string]any, len(idx.Plugins))
	for _, p := range idx.Plugins {
		if id := JSONField(p, "id"); id != "" {
			out[id] = p
		}
	}
	return out
}

// pluginIDs 列出插件源码目录下的所有插件 id（有 plugin.json 的目录）。
func (s *Server) pluginIDs() ([]string, error) {
	entries, err := os.ReadDir(s.cfg.SrcDir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.cfg.SrcDir, e.Name(), "plugin.json")); err == nil {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// listPlugins 汇总源码状态与线上状态。
//
// 中间调了一次 dry-run 打包：它能算出「现在打出来的包哈希是多少」，
// 与线上索引里的哈希一比，就知道哪些插件改了但没发。这比让用户自己记
// 「我改过哪个」可靠得多。
func (s *Server) listPlugins() ([]pluginView, error) {
	ids, err := s.pluginIDs()
	if err != nil {
		return nil, err
	}
	index := readIndex(s.cfg.SiteDir)

	built := map[string]pack.PluginResult{}
	if res, err := pack.PackPlugins(s.cfg.SrcDir, s.cfg.SiteDir, s.releaseKey, true); err == nil {
		for _, p := range res.Plugins {
			built[p.ID] = p
		}
	}

	out := make([]pluginView, 0, len(ids))
	for _, id := range ids {
		raw, err := os.ReadFile(filepath.Join(s.cfg.SrcDir, id, "plugin.json"))
		if err != nil {
			continue
		}
		var mf map[string]any
		if err := json.Unmarshal(raw, &mf); err != nil {
			// 坏掉的清单也要出现在列表里，否则用户看不到自己刚改坏了什么
			out = append(out, pluginView{ID: id, Manifest: map[string]any{"id": id}, Drift: "broken"})
			continue
		}
		v := pluginView{ID: id, Manifest: mf}

		if p, ok := index[id]; ok {
			if pkg, ok := p["package"].(map[string]any); ok {
				v.Published = publishedInfo{
					Version: JSONField(p, "version"),
					Size:    intOf(pkg["size"]),
					SHA256:  JSONField(pkg, "sha256"),
					URL:     JSONField(pkg, "url"),
					Exists:  true,
				}
			}
		}
		if w, ok := built[id]; ok {
			switch {
			case !v.Published.Exists:
				v.Drift = "unpublished"
			case v.Published.Version != w.Version:
				v.Drift = "version"
			case v.Published.SHA256 != w.SHA256:
				v.Drift = "content"
			}
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) pluginDetail(id string) (*pluginView, error) {
	if !validID(id) {
		return nil, fmt.Errorf("插件 id 不合法")
	}
	dir := filepath.Join(s.cfg.SrcDir, id)
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return nil, fmt.Errorf("读 plugin.json 失败：%w", err)
	}
	var mf map[string]any
	if err := json.Unmarshal(raw, &mf); err != nil {
		return nil, fmt.Errorf("plugin.json 不是合法 JSON：%w", err)
	}
	v := &pluginView{ID: id, Manifest: mf, Raw: string(raw)}

	if p, ok := readIndex(s.cfg.SiteDir)[id]; ok {
		if pkg, ok := p["package"].(map[string]any); ok {
			v.Published = publishedInfo{
				Version: JSONField(p, "version"),
				Size:    intOf(pkg["size"]),
				SHA256:  JSONField(pkg, "sha256"),
				URL:     JSONField(pkg, "url"),
				Exists:  true,
			}
		}
	}
	if res, err := pack.PackPlugins(s.cfg.SrcDir, s.cfg.SiteDir, s.releaseKey, true); err == nil {
		for _, p := range res.Plugins {
			if p.ID != id {
				continue
			}
			switch {
			case !v.Published.Exists:
				v.Drift = "unpublished"
			case v.Published.Version != p.Version:
				v.Drift = "version"
			case v.Published.SHA256 != p.SHA256:
				v.Drift = "content"
			}
		}
	}
	if files, err := listFiles(dir); err == nil {
		v.Files = files
	}
	return v, nil
}

func listFiles(root string) ([]fileInfo, error) {
	var out []fileInfo
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, fileInfo{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	return out, err
}

// validateManifest 校验将要写入的清单。
//
// 这些检查在打包时还会做一遍，这里做是为了让错误出现在编辑器边上，
// 而不是等到点「发布」才报。
func validateManifest(id string, mf map[string]any) error {
	if got := JSONField(mf, "id"); got != id {
		return fmt.Errorf("id 必须是 %q（目录名），当前是 %q", id, got)
	}
	ver := JSONField(mf, "version")
	if !version.Valid(ver) {
		return fmt.Errorf("version %q 不是合法版本号（形如 1.0.0）", ver)
	}
	switch name := mf["name"].(type) {
	case string:
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("name 不能为空")
		}
	case map[string]any:
		if len(name) == 0 {
			return fmt.Errorf("name 不能为空")
		}
	default:
		return fmt.Errorf("name 必须是字符串或多语言对象")
	}
	if tier, ok := mf["tier"].(string); ok && tier != "" {
		if tier != "free" && tier != "paid" {
			return fmt.Errorf("tier 只能是 free 或 paid")
		}
	}
	if v, ok := mf["priceCny"]; ok {
		if intOf(v) < 0 {
			return fmt.Errorf("priceCny 不能为负")
		}
	}
	return nil
}

// ---------------------------------------------------------------- 路由

func (s *Server) routePlugins(w http.ResponseWriter, r *http.Request, parts []string) {
	// /api/plugins            列表
	// /api/plugins/<id>       详情
	// /api/plugins/<id>/manifest  PUT
	// /api/plugins/<id>/version   POST
	id := ""
	if len(parts) > 2 {
		id = parts[2]
	}
	action := ""
	if len(parts) > 3 {
		action = parts[3]
	}

	switch {
	case id == "" && r.Method == http.MethodGet:
		list, err := s.listPlugins()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"plugins": list})

	case id != "" && action == "" && r.Method == http.MethodGet:
		v, err := s.pluginDetail(id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, v)

	case id != "" && action == "manifest" && r.Method == http.MethodPut:
		s.handleManifestSave(w, r, id)

	case id != "" && action == "version" && r.Method == http.MethodPost:
		s.handleVersionBump(w, r, id)

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的操作"})
	}
}

func (s *Server) handleManifestSave(w http.ResponseWriter, r *http.Request, id string) {
	if !validID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "插件 id 不合法"})
		return
	}
	var req struct {
		JSON string `json:"json"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var mf map[string]any
	if err := json.Unmarshal([]byte(req.JSON), &mf); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不是合法 JSON：" + err.Error()})
		return
	}
	if err := validateManifest(id, mf); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	target := filepath.Join(s.cfg.SrcDir, id, "plugin.json")
	old, err := os.ReadFile(target)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "读原文件失败：" + err.Error()})
		return
	}
	body, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	body = append(body, '\n')
	if _, err := s.backup("plugins-src", path.Join(id, "plugin.json"), old); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "备份失败，已放弃写入：" + err.Error()})
		return
	}
	if err := os.WriteFile(target, body, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "写入失败：" + err.Error()})
		return
	}
	s.audit.Add(clientIP(r, s.cfg.BehindProxy), "plugin.manifest", id,
		fmt.Sprintf("v%s → v%s", JSONField(mustJSON(old), "version"), JSONField(mf, "version")))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "json": string(body)})
}

func (s *Server) handleVersionBump(w http.ResponseWriter, r *http.Request, id string) {
	if !validID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "插件 id 不合法"})
		return
	}
	var req struct {
		Mode  string `json:"mode"`  // patch | minor | major | set
		Value string `json:"value"` // mode=set 时使用
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	target := filepath.Join(s.cfg.SrcDir, id, "plugin.json")
	old, err := os.ReadFile(target)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	var mf map[string]any
	if err := json.Unmarshal(old, &mf); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "plugin.json 已损坏：" + err.Error()})
		return
	}
	cur := JSONField(mf, "version")
	var next string
	if req.Mode == "set" {
		next = strings.TrimSpace(req.Value)
	} else {
		next, err = bumpVersion(cur, req.Mode)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	if !version.Valid(next) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "版本号不合法：" + next})
		return
	}
	if version.Compare(next, cur) <= 0 {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": fmt.Sprintf("新版本 %s 必须大于当前版本 %s（面板按版本号判断是否要更新）", next, cur)})
		return
	}
	mf["version"] = next
	body, _ := json.MarshalIndent(mf, "", "  ")
	body = append(body, '\n')
	if _, err := s.backup("plugins-src", path.Join(id, "plugin.json"), old); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "备份失败：" + err.Error()})
		return
	}
	if err := os.WriteFile(target, body, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "写入失败：" + err.Error()})
		return
	}
	s.audit.Add(clientIP(r, s.cfg.BehindProxy), "plugin.version", id, cur+" → "+next)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": next, "json": string(body)})
}

// bumpVersion 按语义化版本号进位。
func bumpVersion(cur, mode string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(cur), "v"), ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("当前版本号 %q 无法自动进位，请手动填写", cur)
	}
	nums := make([]int, 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return "", fmt.Errorf("当前版本号 %q 无法自动进位，请手动填写", cur)
		}
		nums[i] = n
	}
	switch mode {
	case "major":
		nums = []int{nums[0] + 1, 0, 0}
	case "minor":
		nums = []int{nums[0], nums[1] + 1, 0}
	case "patch", "":
		nums = []int{nums[0], nums[1], nums[2] + 1}
	default:
		return "", fmt.Errorf("未知的进位方式：%s", mode)
	}
	return fmt.Sprintf("%d.%d.%d", nums[0], nums[1], nums[2]), nil
}

// ---------------------------------------------------------------- 市场打包

func (s *Server) routeMarket(w http.ResponseWriter, r *http.Request, action string) {
	if action != "build" || r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的操作"})
		return
	}
	var req struct {
		DryRun bool `json:"dryRun"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	res, err := pack.PackPlugins(s.cfg.SrcDir, s.cfg.SiteDir, s.releaseKey, req.DryRun)
	if err != nil {
		s.audit.Add(clientIP(r, s.cfg.BehindProxy), "market.build.fail", "", err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	verb := "发布"
	if req.DryRun {
		verb = "演练"
	}
	s.audit.Add(clientIP(r, s.cfg.BehindProxy), "market.build", fmt.Sprintf("%d 个插件", len(res.Plugins)),
		verb+"，索引 "+map[string]string{"same": "未变", "changed": "已更新"}[res.IndexStatus])
	writeJSON(w, http.StatusOK, res)
}

func validID(id string) bool {
	if id == "" || len(id) > 40 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// intOf 把 JSON 里的数字转成 int（可能是 float64）。
func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func mustJSON(raw []byte) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}
