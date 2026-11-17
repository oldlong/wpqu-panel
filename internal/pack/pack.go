// Package pack：把插件源码打成签名包，并生成带签名的市场索引与升级清单。
//
// 这是发布链路上唯一会写 site/ 的地方。命令行工具 wpqupack 和管理后台
// wpquadmin 都调它 —— 两边各写一套打包逻辑，迟早会出现「后台发的包和命令行
// 发的包哈希不同」这种没人查得动的问题。
//
// 所有生成物都带同名 .sig，签的是文件的原始字节。这个约定很重要：
// 签名与落盘必须用同一段 []byte，任何「先签名后追加字节」都会让面板报
// 「签名校验失败」，而且看起来像是被中间人改了。
package pack

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"wpqu-panel/internal/signing"
)

// ---------------------------------------------------------------- 插件

// PluginResult 描述一个插件的打包结果。
type PluginResult struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Size    int    `json:"size"`
	Files   int    `json:"files"`
	SHA256  string `json:"sha256"`
	// Status：new 首次发布 / changed 内容有变 / same 与线上完全一致
	Status string `json:"status"`
}

// PluginsResult 是一次市场打包的汇总。
type PluginsResult struct {
	Plugins     []PluginResult `json:"plugins"`
	IndexPath   string         `json:"indexPath"`
	IndexStatus string         `json:"indexStatus"`
	Skipped     []string       `json:"skipped"`
}

// PackPlugins 打包 srcDir 下所有插件目录，并把带签名的 index.json 写到
// siteDir/api/plugins/。
//
// dryRun 为真时只计算不落盘，用来在改价、改版之前看清会发生什么。
func PackPlugins(srcDir, siteDir string, priv ed25519.PrivateKey, dryRun bool) (*PluginsResult, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, fmt.Errorf("读插件目录失败：%w", err)
	}

	res := &PluginsResult{Plugins: []PluginResult{}, Skipped: []string{}}

	var catalog []map[string]any
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(srcDir, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
		if err != nil {
			res.Skipped = append(res.Skipped, e.Name()+"（没有 plugin.json）")
			continue
		}
		var mf map[string]any
		if err := json.Unmarshal(raw, &mf); err != nil {
			return nil, fmt.Errorf("%s/plugin.json 不是合法 JSON：%w", e.Name(), err)
		}
		id, _ := mf["id"].(string)
		ver, _ := mf["version"].(string)
		if id == "" || ver == "" {
			return nil, fmt.Errorf("%s 缺少 id 或 version", e.Name())
		}
		if id != e.Name() {
			return nil, fmt.Errorf("目录名 %s 与 id %s 不一致（面板按 id 找目录）", e.Name(), id)
		}

		files, err := collectFiles(dir)
		if err != nil {
			return nil, fmt.Errorf("收集 %s 文件失败：%w", id, err)
		}
		if _, ok := files["plugin.json"]; !ok {
			return nil, fmt.Errorf("%s 的 plugin.json 必须在包根目录", id)
		}

		zipBytes, err := buildZip(files)
		if err != nil {
			return nil, fmt.Errorf("打包 %s 失败：%w", id, err)
		}
		sum := sha256.Sum256(zipBytes)
		sumHex := hex.EncodeToString(sum[:])

		outDir := filepath.Join(siteDir, "plugins", id)
		zipPath := filepath.Join(outDir, ver+".zip")
		status := "new"
		if old, err := os.ReadFile(zipPath); err == nil {
			if bytes.Equal(old, zipBytes) {
				status = "same"
			} else {
				status = "changed"
			}
		}

		if !dryRun {
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return nil, fmt.Errorf("创建目录失败：%w", err)
			}
			if err := os.WriteFile(zipPath, zipBytes, 0o644); err != nil {
				return nil, fmt.Errorf("写包失败：%w", err)
			}
			if err := signing.WriteSignature(zipPath, zipBytes, priv); err != nil {
				return nil, fmt.Errorf("签名失败：%w", err)
			}
		}

		mf["package"] = map[string]any{
			"url":    fmt.Sprintf("/plugins/%s/%s.zip", id, ver),
			"sha256": sumHex,
			"size":   len(zipBytes),
		}
		catalog = append(catalog, mf)
		res.Plugins = append(res.Plugins, PluginResult{
			ID: id, Version: ver, Size: len(zipBytes),
			Files: len(files), SHA256: sumHex, Status: status,
		})
	}

	sort.Slice(catalog, func(i, j int) bool {
		ci, _ := catalog[i]["category"].(string)
		cj, _ := catalog[j]["category"].(string)
		if ci != cj {
			return ci < cj
		}
		return fmt.Sprint(catalog[i]["id"]) < fmt.Sprint(catalog[j]["id"])
	})

	index := map[string]any{
		"schema":  1,
		"updated": time.Now().Format("2006-01-02"),
		"plugins": catalog,
	}
	body, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n') // 签名对象必须与落盘字节完全一致

	apiDir := filepath.Join(siteDir, "api", "plugins")
	indexPath := filepath.Join(apiDir, "index.json")
	res.IndexPath = indexPath
	// 只比插件内容，不比 updated 日期：否则跨天之后每次演练都报「索引将变化」，
	// 而这个提示的意义恰恰是「有没有实质改动」。
	res.IndexStatus = "changed"
	if old, err := os.ReadFile(indexPath); err == nil && sameCatalog(old, catalog) {
		res.IndexStatus = "same"
	}
	if dryRun {
		return res, nil
	}
	if err := os.MkdirAll(apiDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 api 目录失败：%w", err)
	}
	if err := os.WriteFile(indexPath, body, 0o644); err != nil {
		return nil, fmt.Errorf("写索引失败：%w", err)
	}
	if err := signing.WriteSignature(indexPath, body, priv); err != nil {
		return nil, fmt.Errorf("签名失败：%w", err)
	}
	return res, nil
}

// sameCatalog 比较索引里的插件清单部分，忽略 updated 日期。
// 只看「实质内容有没有变」，和界面上的「索引将变化」保持同一套判断。
// 两侧都先过一遍 JSON 归一化（键排序、数字统一成 float64），避免 int/float 字面差异造成误判。
func sameCatalog(old []byte, catalog []map[string]any) bool {
	var prev struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(old, &prev); err != nil {
		return false
	}
	a, err := json.Marshal(prev.Plugins)
	if err != nil {
		return false
	}
	raw, err := json.Marshal(catalog)
	if err != nil {
		return false
	}
	var b []byte
	if b, err = normalizeJSON(raw); err != nil {
		return false
	}
	return bytes.Equal(a, b)
}

func normalizeJSON(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// collectFiles 读目录下所有文件，路径用 / 分隔且相对包根。
func collectFiles(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || name == "Thumbs.db" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// 脚本一律转 LF：Windows 上编辑过的文件带 CRLF，远端 bash 会报 $'\r' 错误
		switch strings.ToLower(filepath.Ext(rel)) {
		case ".sh", ".js", ".mjs", ".json", ".md", ".conf", ".service":
			raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		}
		files[rel] = raw
		return nil
	})
	return files, err
}

func buildZip(files map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// 固定顺序与时间戳，保证同样内容打出的哈希稳定
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h := &zip.FileHeader{Name: k, Method: zip.Deflate}
		h.SetMode(0o644)
		if strings.HasSuffix(k, ".sh") {
			h.SetMode(0o755)
		}
		h.Modified = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(files[k]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------- 升级

// 只认这三类平台的可执行文件；console 版与 SHA256SUMS 不进升级清单。
var binRe = regexp.MustCompile(`^wpqu-panel-(windows|linux|darwin)-(amd64|arm64|386)(\.exe)?$`)

// DistFile 是 dist 里一个可发布的二进制。
type DistFile struct {
	Name    string `json:"name"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Size    int64  `json:"size"`
	ModTime string `json:"mtime"`
}

// ListBinaries 列出 dist 目录里能被升级清单收录的二进制。
//
// 后台的「发布升级」页面用它给用户看清楚「这次会发出去哪几个文件」，
// 免得把 console 版或半成品的构建也一起发了。
func ListBinaries(distDir string) ([]DistFile, error) {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return nil, err
	}
	out := []DistFile{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := binRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, DistFile{
			Name: e.Name(), OS: m[1], Arch: m[2],
			Size: info.Size(), ModTime: info.ModTime().Format("2006-01-02 15:04"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UpdatesOptions 描述一次升级发布。
type UpdatesOptions struct {
	DistDir      string              // 构建产物目录
	SiteDir      string              // 站点目录
	Version      string              // 例如 1.9.0
	MinSupported string              // 低于此版本不再提示升级，空则沿用清单里的值
	Critical     bool                // 重大更新：面板会标红提示
	Notes        map[string][]string // 发布说明，键为语言代码
}

// UpdatesResult 是发布结果。
type UpdatesResult struct {
	Version   string   `json:"version"`
	Platforms int      `json:"platforms"`
	Versions  int      `json:"versions"`
	Files     []string `json:"files"`
	Path      string   `json:"path"`
}

// PackUpdates 把 dist 里的二进制复制进 site/dl/<version>/，并更新签名清单。
func PackUpdates(opt UpdatesOptions, priv ed25519.PrivateKey) (*UpdatesResult, error) {
	if strings.TrimSpace(opt.Version) == "" {
		return nil, errors.New("需要版本号")
	}
	entries, err := os.ReadDir(opt.DistDir)
	if err != nil {
		return nil, fmt.Errorf("读构建目录失败：%w", err)
	}
	dlDir := filepath.Join(opt.SiteDir, "dl", opt.Version)
	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建下载目录失败：%w", err)
	}

	var files []map[string]any
	res := &UpdatesResult{Version: opt.Version, Files: []string{}}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := binRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(opt.DistDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("读 %s 失败：%w", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dlDir, e.Name()), raw, 0o644); err != nil {
			return nil, fmt.Errorf("复制 %s 失败：%w", e.Name(), err)
		}
		sum := sha256.Sum256(raw)
		files = append(files, map[string]any{
			"os":     m[1],
			"arch":   m[2],
			"url":    "/dl/" + opt.Version + "/" + e.Name(),
			"sha256": hex.EncodeToString(sum[:]),
			"size":   len(raw),
		})
		res.Files = append(res.Files, e.Name())
	}
	if len(files) == 0 {
		return nil, errors.New("dist 里没有可发布的二进制（文件名要形如 wpqu-panel-linux-amd64）")
	}
	res.Platforms = len(files)

	apiDir := filepath.Join(opt.SiteDir, "api")
	if err := os.MkdirAll(apiDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 api 目录失败：%w", err)
	}
	updPath := filepath.Join(apiDir, "updates.json")
	res.Path = updPath

	// 保留历史版本，方便跳版升级时看到中间的更新说明
	var manifest struct {
		Schema       int              `json:"schema"`
		Channel      string           `json:"channel"`
		Latest       string           `json:"latest"`
		MinSupported string           `json:"minSupported"`
		Releases     []map[string]any `json:"releases"`
	}
	if raw, err := os.ReadFile(updPath); err == nil {
		_ = json.Unmarshal(raw, &manifest)
	}
	if manifest.Schema == 0 {
		manifest.Schema = 1
		manifest.Channel = "stable"
	}
	manifest.Latest = opt.Version
	if opt.MinSupported != "" {
		manifest.MinSupported = opt.MinSupported
	}
	if manifest.MinSupported == "" {
		manifest.MinSupported = "1.9.0"
	}

	notes := opt.Notes
	if notes == nil {
		notes = map[string][]string{}
	}
	rel := map[string]any{
		"version":  opt.Version,
		"date":     time.Now().Format("2006-01-02"),
		"critical": opt.Critical,
		"notes":    notes,
		"files":    files,
	}
	kept := []map[string]any{rel}
	for _, old := range manifest.Releases {
		if fmt.Sprint(old["version"]) == opt.Version {
			continue
		}
		if len(kept) >= 20 {
			break
		}
		kept = append(kept, old)
	}
	manifest.Releases = kept
	res.Versions = len(kept)

	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n') // 同上：签的字节要和写下去的字节一致
	if err := os.WriteFile(updPath, body, 0o644); err != nil {
		return nil, fmt.Errorf("写升级清单失败：%w", err)
	}
	if err := signing.WriteSignature(updPath, body, priv); err != nil {
		return nil, fmt.Errorf("签名失败：%w", err)
	}
	return res, nil
}
