package admin

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 允许在后台里编辑的文本文件类型。
//
// 白名单而不是黑名单：这个目录里有 .keys 私钥，还有几 MB 的二进制包，
// 用「排除法」迟早会漏一个。
var textExt = map[string]bool{
	".html": true, ".htm": true, ".css": true, ".js": true, ".mjs": true,
	".json": true, ".md": true, ".svg": true, ".txt": true, ".sh": true,
	".conf": true, ".service": true, ".yml": true, ".yaml": true,
	".gitignore": true, ".htaccess": true,
}

const (
	maxReadSize  = 1 << 20 // 1 MB
	maxWriteSize = 512 << 10
)

// rootDir 把逻辑根名映射到真实目录。
func (s *Server) rootDir(name string) (string, error) {
	switch name {
	case "site":
		return s.cfg.SiteDir, nil
	case "plugins-src":
		return s.cfg.SrcDir, nil
	}
	return "", errors.New("未知的根目录")
}

// safeRel 拒绝任何可能跑出根目录的路径。
func safeRel(rel string) (string, error) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return "", errors.New("缺少文件路径")
	}
	if strings.Contains(rel, "..") || strings.ContainsRune(rel, 0) {
		return "", errors.New("路径不合法")
	}
	clean := path.Clean(rel)
	if clean == "." || strings.HasPrefix(clean, "../") {
		return "", errors.New("路径不合法")
	}
	// 私钥永远不给看，即便有人把 keysDir 配到了站点目录里
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".keys" || strings.HasSuffix(seg, ".key") {
			return "", errors.New("这个文件不允许通过后台访问")
		}
	}
	return clean, nil
}

// resolve 算出磁盘上的真实路径，并确认扩展名在白名单里。
func (s *Server) resolve(rootName, rel string) (dir, abs string, err error) {
	dir, err = s.rootDir(rootName)
	if err != nil {
		return "", "", err
	}
	clean, err := safeRel(rel)
	if err != nil {
		return "", "", err
	}
	abs = filepath.Join(dir, filepath.FromSlash(clean))
	// 二次确认：Clean 之后仍在根目录内
	if !strings.HasPrefix(abs, dir+string(os.PathSeparator)) {
		return "", "", errors.New("路径越界")
	}
	// site/api 下的文件是签名过的生成物。手改一个字节，签名就作废，
	// 所有面板都会拒绝这个市场 —— 必须挡住，而不是等客户装不上才发现。
	if rootName == "site" && (clean == "api" || strings.HasPrefix(clean, "api/")) {
		return "", "", errors.New("api/ 下是自动生成的签名文件，请用「插件市场」或「升级发布」重新生成")
	}
	if !textExt[strings.ToLower(filepath.Ext(clean))] {
		return "", "", fmt.Errorf("不能通过后台编辑 %s 类型的文件", filepath.Ext(clean))
	}
	return dir, abs, nil
}

// ---------------------------------------------------------------- 备份

type backupInfo struct {
	Name string `json:"name"`
	At   string `json:"at"`
	Size int64  `json:"size"`
}

// backup 把即将被覆盖的旧内容存一份，返回备份文件名。
func (s *Server) backup(rootName, rel string, old []byte) (string, error) {
	clean, err := safeRel(rel)
	if err != nil {
		return "", err
	}
	ts := time.Now().Format("20060102-150405.000")
	name := path.Base(clean) + "." + ts + ".bak"
	target := filepath.Join(s.cfg.DataDir, "backups", rootName,
		filepath.FromSlash(path.Dir(clean)), name)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(target, old, 0o600); err != nil {
		return "", err
	}
	return name, nil
}

// listBackups 列出某个文件的备份，新的在前。
func (s *Server) listBackups(rootName, rel string) []backupInfo {
	out := []backupInfo{}
	clean, err := safeRel(rel)
	if err != nil {
		return out
	}
	dir := filepath.Join(s.cfg.DataDir, "backups", rootName, filepath.FromSlash(path.Dir(clean)))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	prefix := path.Base(clean) + "."
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".bak") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, backupInfo{
			Name: e.Name(),
			At:   info.ModTime().Format("2006-01-02 15:04:05"),
			Size: info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// ---------------------------------------------------------------- 路由

func (s *Server) routeSite(w http.ResponseWriter, r *http.Request, action string) {
	switch action {
	case "tree":
		s.handleTree(w, r)
	case "file":
		if r.Method == http.MethodPut {
			s.handleFileSave(w, r)
			return
		}
		s.handleFileRead(w, r)
	case "backups":
		s.handleBackups(w, r)
	case "restore":
		s.handleRestore(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "没有这个接口"})
	}
}

type treeEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"mtime"`
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	rootName := r.URL.Query().Get("root")
	if rootName == "" {
		rootName = "site"
	}
	dir, err := s.rootDir(rootName)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	out := []treeEntry{}
	count := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个目录读不了不该让整棵树消失
		}
		name := d.Name()
		if d.IsDir() {
			// dl 是二进制、api 是签名生成物，都不该出现在可编辑列表里
			if name == ".git" || name == ".keys" || name == "node_modules" || name == "dl" || name == "api" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") && name != ".gitignore" && name != ".htaccess" {
			return nil
		}
		if !textExt[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, treeEntry{
			Path:    filepath.ToSlash(rel),
			Size:    info.Size(),
			ModTime: info.ModTime().Format("2006-01-02 15:04"),
		})
		count++
		if count > 3000 {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	writeJSON(w, http.StatusOK, map[string]any{"root": rootName, "files": out})
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request) {
	rootName := r.URL.Query().Get("root")
	rel := r.URL.Query().Get("path")
	_, abs, err := s.resolve(rootName, rel)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件不存在"})
		return
	}
	if info.Size() > maxReadSize {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "文件太大，请用命令行编辑"})
		return
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"root": rootName, "path": rel,
		"content": string(body),
		"size":    info.Size(),
		"mtime":   info.ModTime().Format("2006-01-02 15:04:05"),
		"backups": s.listBackups(rootName, rel),
	})
}

func (s *Server) handleFileSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Root    string `json:"root"`
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(req.Content) > maxWriteSize {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "内容超过 512 KB，请用命令行编辑"})
		return
	}
	_, abs, err := s.resolve(req.Root, req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var backupName string
	if old, err := os.ReadFile(abs); err == nil {
		// 内容没变就别留一堆无意义的备份
		if string(old) == req.Content {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unchanged": true})
			return
		}
		name, err := s.backup(req.Root, req.Path, old)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "备份失败，已放弃写入：" + err.Error()})
			return
		}
		backupName = name
	}
	if err := os.WriteFile(abs, []byte(req.Content), 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "写入失败：" + err.Error()})
		return
	}
	s.audit.Add(clientIP(r, s.cfg.BehindProxy), "site.save",
		req.Root+"/"+req.Path, fmt.Sprintf("%d 字节", len(req.Content)))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "backup": backupName, "backups": s.listBackups(req.Root, req.Path),
	})
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	p := r.URL.Query().Get("path")
	writeJSON(w, http.StatusOK, map[string]any{"backups": s.listBackups(root, p)})
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Root   string `json:"root"`
		Path   string `json:"path"`
		Backup string `json:"backup"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	_, abs, err := s.resolve(req.Root, req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	clean, err := safeRel(req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 备份文件名只允许是「目标文件名.xxx.bak」，防止用它去读别的文件
	base := path.Base(clean)
	if !strings.HasPrefix(req.Backup, base+".") || !strings.HasSuffix(req.Backup, ".bak") ||
		strings.Contains(req.Backup, "/") || strings.Contains(req.Backup, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "备份文件名不合法"})
		return
	}
	src := filepath.Join(s.cfg.DataDir, "backups", req.Root,
		filepath.FromSlash(path.Dir(clean)), req.Backup)
	old, err := os.ReadFile(src)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "备份不存在"})
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// 还原前把当前内容也存一份，还原错了还能退回去
	if cur, err := os.ReadFile(abs); err == nil {
		_, _ = s.backup(req.Root, req.Path, cur)
	}
	if err := os.WriteFile(abs, old, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.audit.Add(clientIP(r, s.cfg.BehindProxy), "site.restore", req.Root+"/"+req.Path, req.Backup)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "content": string(old)})
}
