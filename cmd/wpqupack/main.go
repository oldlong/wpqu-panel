// wpqupack：把插件源码打成签名包，并生成带签名的市场索引与升级清单。
//
//	go run ./cmd/wpqupack plugins -src ../plugins-src -site ../site -key ../site/.keys/release.key
//	go run ./cmd/wpqupack updates -dist ../dist -site ../site -version 1.9.0 -key ../site/.keys/release.key
//
// 生成物（全部带 .sig，面板只认签名过的）：
//
//	site/plugins/<id>/<version>.zip
//	site/api/plugins/index.json
//	site/api/updates.json
//	site/dl/<version>/<binary>
package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "plugins":
		cmdPlugins(os.Args[2:])
	case "updates":
		cmdUpdates(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `wpqupack — WPQU panel 打包工具

  plugins   打包 plugins-src/* 并生成市场索引
  updates   收集 dist/* 二进制并生成升级清单
`)
	os.Exit(2)
}

// ---------------------------------------------------------------- plugins

func cmdPlugins(args []string) {
	fs := flag.NewFlagSet("plugins", flag.ExitOnError)
	src := fs.String("src", "../plugins-src", "插件源码目录")
	site := fs.String("site", "../site", "站点目录")
	keyFile := fs.String("key", "", "发布私钥")
	_ = fs.Parse(args)
	if *keyFile == "" {
		die("需要 -key 指定发布私钥")
	}
	priv := loadKey(*keyFile)

	entries, err := os.ReadDir(*src)
	if err != nil {
		die("读插件目录失败：%v", err)
	}

	var catalog []map[string]any
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(*src, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
		if err != nil {
			fmt.Printf("  跳过 %s（没有 plugin.json）\n", e.Name())
			continue
		}
		var mf map[string]any
		if err := json.Unmarshal(raw, &mf); err != nil {
			die("%s/plugin.json 不是合法 JSON：%v", e.Name(), err)
		}
		id, _ := mf["id"].(string)
		ver, _ := mf["version"].(string)
		if id == "" || ver == "" {
			die("%s 缺少 id 或 version", e.Name())
		}
		if id != e.Name() {
			die("目录名 %s 与 id %s 不一致（面板按 id 找目录）", e.Name(), id)
		}

		files, err := collectFiles(dir)
		if err != nil {
			die("收集 %s 文件失败：%v", id, err)
		}
		if _, ok := files["plugin.json"]; !ok {
			die("%s 的 plugin.json 必须在包根目录", id)
		}

		zipBytes, err := buildZip(files)
		if err != nil {
			die("打包 %s 失败：%v", id, err)
		}

		outDir := filepath.Join(*site, "plugins", id)
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			die("创建目录失败：%v", err)
		}
		zipPath := filepath.Join(outDir, ver+".zip")
		if err := os.WriteFile(zipPath, zipBytes, 0o644); err != nil {
			die("写包失败：%v", err)
		}
		if err := writeSigBytes(zipPath, zipBytes, priv); err != nil {
			die("签名失败：%v", err)
		}

		sum := sha256.Sum256(zipBytes)
		mf["package"] = map[string]any{
			"url":    fmt.Sprintf("/plugins/%s/%s.zip", id, ver),
			"sha256": hex.EncodeToString(sum[:]),
			"size":   len(zipBytes),
		}
		catalog = append(catalog, mf)
		fmt.Printf("  ✓ %-10s v%-8s %6.1f KB  %d 个文件\n", id, ver, float64(len(zipBytes))/1024, len(files))
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
	apiDir := filepath.Join(*site, "api", "plugins")
	if err := os.MkdirAll(apiDir, 0o755); err != nil {
		die("创建 api 目录失败：%v", err)
	}
	body, _ := json.MarshalIndent(index, "", "  ")
	body = append(body, '\n') // 签名对象必须与落盘字节完全一致
	path := filepath.Join(apiDir, "index.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		die("写索引失败：%v", err)
	}
	if err := writeSigBytes(path, body, priv); err != nil {
		die("签名失败：%v", err)
	}
	fmt.Printf("\n索引：%s（%d 个插件）\n", path, len(catalog))
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
	// 固定顺序，保证同样内容打出的哈希稳定
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

// ---------------------------------------------------------------- updates

var binRe = regexp.MustCompile(`^wpqu-panel-(windows|linux|darwin)-(amd64|arm64|386)(\.exe)?$`)

func cmdUpdates(args []string) {
	fs := flag.NewFlagSet("updates", flag.ExitOnError)
	dist := fs.String("dist", "../dist", "构建产物目录")
	site := fs.String("site", "../site", "站点目录")
	ver := fs.String("version", "", "本次版本号，例如 1.9.0")
	keyFile := fs.String("key", "", "发布私钥")
	notesFile := fs.String("notes", "", "发布说明 JSON（可选）")
	_ = fs.Parse(args)
	if *ver == "" || *keyFile == "" {
		die("需要 -version 与 -key")
	}
	priv := loadKey(*keyFile)

	entries, err := os.ReadDir(*dist)
	if err != nil {
		die("读构建目录失败：%v", err)
	}
	dlDir := filepath.Join(*site, "dl", *ver)
	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		die("创建下载目录失败：%v", err)
	}

	var files []map[string]any
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := binRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue // console 版与校验文件不进升级清单
		}
		raw, err := os.ReadFile(filepath.Join(*dist, e.Name()))
		if err != nil {
			die("读 %s 失败：%v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dlDir, e.Name()), raw, 0o644); err != nil {
			die("复制 %s 失败：%v", e.Name(), err)
		}
		sum := sha256.Sum256(raw)
		files = append(files, map[string]any{
			"os":     m[1],
			"arch":   m[2],
			"url":    "/dl/" + *ver + "/" + e.Name(),
			"sha256": hex.EncodeToString(sum[:]),
			"size":   len(raw),
		})
		fmt.Printf("  ✓ %-40s %6.1f MB\n", e.Name(), float64(len(raw))/(1<<20))
	}
	if len(files) == 0 {
		die("dist 里没有可发布的二进制（文件名要形如 wpqu-panel-linux-amd64）")
	}

	notes := map[string][]string{}
	if *notesFile != "" {
		raw, err := os.ReadFile(*notesFile)
		if err != nil {
			die("读发布说明失败：%v", err)
		}
		if err := json.Unmarshal(raw, &notes); err != nil {
			die("发布说明格式应为 {\"zh-CN\":[\"...\"],\"en\":[\"...\"]}：%v", err)
		}
	}

	apiDir := filepath.Join(*site, "api")
	if err := os.MkdirAll(apiDir, 0o755); err != nil {
		die("创建 api 目录失败：%v", err)
	}
	updPath := filepath.Join(apiDir, "updates.json")

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
	manifest.Latest = *ver
	if manifest.MinSupported == "" {
		manifest.MinSupported = "1.9.0"
	}

	rel := map[string]any{
		"version":  *ver,
		"date":     time.Now().Format("2006-01-02"),
		"critical": false,
		"notes":    notes,
		"files":    files,
	}
	kept := []map[string]any{rel}
	for _, old := range manifest.Releases {
		if fmt.Sprint(old["version"]) == *ver {
			continue
		}
		if len(kept) >= 20 {
			break
		}
		kept = append(kept, old)
	}
	manifest.Releases = kept

	body, _ := json.MarshalIndent(manifest, "", "  ")
	body = append(body, '\n') // 同上：签的字节要和写下去的字节一致
	if err := os.WriteFile(updPath, body, 0o644); err != nil {
		die("写升级清单失败：%v", err)
	}
	if err := writeSigBytes(updPath, body, priv); err != nil {
		die("签名失败：%v", err)
	}
	fmt.Printf("\n升级清单：%s（%d 个平台，共 %d 个版本）\n", updPath, len(files), len(kept))
}

// ---------------------------------------------------------------- 工具

func writeSigBytes(path string, data []byte, priv ed25519.PrivateKey) error {
	sig := ed25519.Sign(priv, data)
	return os.WriteFile(path+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644)
}

func loadKey(path string) ed25519.PrivateKey {
	raw, err := os.ReadFile(path)
	if err != nil {
		die("读私钥失败：%v", err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		die("私钥不是合法 base64：%v", err)
	}
	switch len(b) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b)
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b)
	}
	die("私钥长度异常：%d", len(b))
	return nil
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
