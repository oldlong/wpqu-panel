package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"wpqu-panel/internal/pack"
)

// releaseView 是升级清单里一个版本的精简形态。
type releaseView struct {
	Version   string   `json:"version"`
	Date      string   `json:"date"`
	Critical  bool     `json:"critical"`
	Platforms int      `json:"platforms"`
	Notes     []string `json:"notes"`
}

func (s *Server) routeUpdates(w http.ResponseWriter, r *http.Request, action string) {
	switch {
	case action == "" && r.Method == http.MethodGet:
		s.handleUpdatesInfo(w, r)
	case action == "publish" && r.Method == http.MethodPost:
		s.handleUpdatesPublish(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的操作"})
	}
}

// readUpdates 读线上清单。文件不存在时返回空结构，让界面能正常显示第一次发布。
func (s *Server) readUpdates() (map[string]any, []releaseView) {
	raw, err := os.ReadFile(filepath.Join(s.cfg.SiteDir, "api", "updates.json"))
	if err != nil {
		return map[string]any{}, nil
	}
	var m struct {
		Schema       int              `json:"schema"`
		Channel      string           `json:"channel"`
		Latest       string           `json:"latest"`
		MinSupported string           `json:"minSupported"`
		Releases     []map[string]any `json:"releases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{"error": "清单已损坏：" + err.Error()}, nil
	}
	out := make([]releaseView, 0, len(m.Releases))
	for _, rel := range m.Releases {
		v := releaseView{
			Version:  JSONField(rel, "version"),
			Date:     JSONField(rel, "date"),
			Critical: rel["critical"] == true,
		}
		if files, ok := rel["files"].([]any); ok {
			v.Platforms = len(files)
		}
		if notes, ok := rel["notes"].(map[string]any); ok {
			for _, lang := range []string{"zh-CN", "zh", "en"} {
				if arr, ok := notes[lang].([]any); ok {
					for _, n := range arr {
						if line, ok := n.(string); ok && strings.TrimSpace(line) != "" {
							v.Notes = append(v.Notes, line)
						}
					}
					break
				}
			}
		}
		out = append(out, v)
	}
	return map[string]any{
		"schema":       m.Schema,
		"channel":      m.Channel,
		"latest":       m.Latest,
		"minSupported": m.MinSupported,
		"updated":      JSONField(mustJSON(raw), "updated"),
	}, out
}

func (s *Server) handleUpdatesInfo(w http.ResponseWriter, r *http.Request) {
	meta, releases := s.readUpdates()
	bins, err := pack.ListBinaries(s.cfg.DistDir)
	if err != nil {
		// dist 不存在是常见状态（还没编译过），不该当作错误
		bins = []pack.DistFile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"manifest":   meta,
		"releases":   releases,
		"distFiles":  bins,
		"distDir":    s.cfg.DistDir,
		"siteReady":  fileExists(filepath.Join(s.cfg.SiteDir, "api", "updates.json")),
		"updateBase": "https://panel.wpqu.com",
	})
}

func (s *Server) handleUpdatesPublish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version      string `json:"version"`
		MinSupported string `json:"minSupported"`
		Critical     bool   `json:"critical"`
		NotesZh      string `json:"notesZh"`
		NotesEn      string `json:"notesEn"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.Version = strings.TrimSpace(strings.TrimPrefix(req.Version, "v"))
	if req.Version == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "需要版本号"})
		return
	}
	notes := map[string][]string{}
	if zh := splitNotes(req.NotesZh); len(zh) > 0 {
		notes["zh-CN"] = zh
	}
	if en := splitNotes(req.NotesEn); len(en) > 0 {
		notes["en"] = en
	}
	if len(notes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "至少写一条更新说明：面板的更新弹窗要显示它"})
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	res, err := pack.PackUpdates(pack.UpdatesOptions{
		DistDir:      s.cfg.DistDir,
		SiteDir:      s.cfg.SiteDir,
		Version:      req.Version,
		MinSupported: strings.TrimSpace(req.MinSupported),
		Critical:     req.Critical,
		Notes:        notes,
	}, s.releaseKey)
	if err != nil {
		s.audit.Add(clientIP(r, s.cfg.BehindProxy), "update.publish.fail", req.Version, err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.audit.Add(clientIP(r, s.cfg.BehindProxy), "update.publish", req.Version,
		fmt.Sprintf("%d 个平台，critical=%v", res.Platforms, req.Critical))
	writeJSON(w, http.StatusOK, res)
}

// splitNotes 把多行文本变成数组，去掉空行与行首的序号符号。
func splitNotes(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*·"))
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
