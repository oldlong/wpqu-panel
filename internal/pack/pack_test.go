package pack

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wpqu-panel/internal/signing"
)

func newKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, base64.StdEncoding.EncodeToString(pub)
}

// writePlugin 造一个最小插件目录。
func writePlugin(t *testing.T, root, id, ver string, extra map[string]string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	mf := map[string]any{
		"id": id, "version": ver, "category": "runtime", "tier": "free",
		"name":    map[string]string{"zh-CN": id, "en": id},
		"install": map[string]any{"steps": []any{}},
	}
	raw, _ := json.MarshalIndent(mf, "", "  ")
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "install.sh"), []byte("#!/bin/sh\necho hi\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range extra {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPackPlugins(t *testing.T) {
	priv, pubB64 := newKey(t)
	src := t.TempDir()
	site := t.TempDir()
	writePlugin(t, src, "alpha", "1.0.0", nil)
	writePlugin(t, src, "beta", "2.1.0", map[string]string{"README.md": "# beta\n"})
	// 没有 plugin.json 的目录应当被跳过而不是报错
	if err := os.MkdirAll(filepath.Join(src, "notes-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := PackPlugins(src, site, priv, false)
	if err != nil {
		t.Fatalf("打包失败：%v", err)
	}
	if len(res.Plugins) != 2 {
		t.Fatalf("应当打包 2 个，实际 %d", len(res.Plugins))
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("应当跳过 1 个：%v", res.Skipped)
	}
	for _, p := range res.Plugins {
		if p.Status != "new" {
			t.Fatalf("%s 首次发布状态应为 new，实际 %s", p.ID, p.Status)
		}
		zipPath := filepath.Join(site, "plugins", p.ID, p.Version+".zip")
		raw, err := os.ReadFile(zipPath)
		if err != nil {
			t.Fatalf("包没落盘：%v", err)
		}
		sig, err := os.ReadFile(zipPath + ".sig")
		if err != nil {
			t.Fatalf("签名没落盘：%v", err)
		}
		if err := signing.VerifyWith(pubB64, raw, string(sig)); err != nil {
			t.Fatalf("%s 包签名验不过：%v", p.ID, err)
		}
	}

	// 索引也必须能验签
	indexPath := filepath.Join(site, "api", "plugins", "index.json")
	body, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(indexPath + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.VerifyWith(pubB64, body, string(sig)); err != nil {
		t.Fatalf("索引签名验不过：%v", err)
	}

	var idx struct {
		Schema  int              `json:"schema"`
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(body, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Plugins) != 2 || idx.Schema != 1 {
		t.Fatalf("索引内容不对：%d 个插件", len(idx.Plugins))
	}
	pkg, _ := idx.Plugins[0]["package"].(map[string]any)
	if pkg["url"] == "" || pkg["sha256"] == "" {
		t.Fatalf("索引缺少 package 信息：%v", pkg)
	}

	// 内容不变时应当是 same
	res2, err := PackPlugins(src, site, priv, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res2.Plugins {
		if p.Status != "same" {
			t.Fatalf("重复打包 %s 应为 same，实际 %s", p.ID, p.Status)
		}
	}

	// 改一个脚本后应当是 changed，且哈希变化
	if err := os.WriteFile(filepath.Join(src, "alpha", "scripts", "install.sh"), []byte("#!/bin/sh\necho bye\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res3, err := PackPlugins(src, site, priv, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res3.Plugins {
		if p.ID == "alpha" && p.Status != "changed" {
			t.Fatalf("改动后应为 changed，实际 %s", p.Status)
		}
		if p.ID == "beta" && p.Status != "same" {
			t.Fatalf("没改的应为 same，实际 %s", p.Status)
		}
	}
}

// CRLF 必须转成 LF，否则远端 bash 会报 $'\r' 错误。
func TestPackNormalizesCRLF(t *testing.T) {
	priv, _ := newKey(t)
	src := t.TempDir()
	site := t.TempDir()
	writePlugin(t, src, "golang", "1.0.0", nil)

	if _, err := PackPlugins(src, site, priv, false); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(site, "plugins", "golang", "1.0.0.zip")
	raw, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\r\n") {
		t.Fatal("包里不应出现 CRLF")
	}
}

func TestPackPluginsIDMismatch(t *testing.T) {
	priv, _ := newKey(t)
	src := t.TempDir()
	site := t.TempDir()
	dir := filepath.Join(src, "wrong-name")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"id": "golang", "version": "1.0.0"})
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PackPlugins(src, site, priv, false); err == nil {
		t.Fatal("目录名与 id 不一致应当报错")
	}
}

func TestPackDryRunWritesNothing(t *testing.T) {
	priv, _ := newKey(t)
	src := t.TempDir()
	site := t.TempDir()
	writePlugin(t, src, "alpha", "1.0.0", nil)

	res, err := PackPlugins(src, site, priv, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plugins) != 1 || res.Plugins[0].Size == 0 {
		t.Fatalf("演练也要算出结果：%+v", res.Plugins)
	}
	if _, err := os.Stat(filepath.Join(site, "plugins")); !os.IsNotExist(err) {
		t.Fatal("演练不应写包")
	}
	if _, err := os.Stat(filepath.Join(site, "api")); !os.IsNotExist(err) {
		t.Fatal("演练不应写索引")
	}
}

// 索引状态只看插件内容，不看 updated 日期：跨天之后演练仍应报「未变」，
// 否则这个提示永远为真，等于没有提示。
func TestPackIndexStatusIgnoresDate(t *testing.T) {
	priv, _ := newKey(t)
	src := t.TempDir()
	site := t.TempDir()
	writePlugin(t, src, "alpha", "1.0.0", nil)

	res, err := PackPlugins(src, site, priv, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.IndexStatus != "changed" {
		t.Fatalf("首次发布应为 changed，实际 %q", res.IndexStatus)
	}

	// 把索引里的日期改成很久以前，模拟「隔天再演练」
	indexPath := filepath.Join(site, "api", "plugins", "index.json")
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var idx map[string]any
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	idx["updated"] = "2001-01-01"
	out, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(indexPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err = PackPlugins(src, site, priv, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.IndexStatus != "same" {
		t.Fatalf("只有日期变化应报 same，实际 %q", res.IndexStatus)
	}

	// 真正改内容（版本进位），必须报 changed
	writePlugin(t, src, "alpha", "1.1.0", nil)
	res, err = PackPlugins(src, site, priv, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.IndexStatus != "changed" {
		t.Fatalf("版本变化应报 changed，实际 %q", res.IndexStatus)
	}
}

func TestPackUpdates(t *testing.T) {
	priv, pubB64 := newKey(t)
	dist := t.TempDir()
	site := t.TempDir()
	for _, name := range []string{"wpqu-panel-linux-amd64", "wpqu-panel-windows-amd64.exe", "wpqu-panel-windows-console.exe", "SHA256SUMS"} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte("bin-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := PackUpdates(UpdatesOptions{
		DistDir: dist, SiteDir: site, Version: "1.9.1", Critical: true,
		Notes: map[string][]string{"zh-CN": {"修了 A"}, "en": {"Fixed A"}},
	}, priv)
	if err != nil {
		t.Fatalf("发布失败：%v", err)
	}
	if res.Platforms != 2 {
		t.Fatalf("应当只收 2 个平台（console 与校验文件不进清单），实际 %d", res.Platforms)
	}
	if _, err := os.Stat(filepath.Join(site, "dl", "1.9.1", "wpqu-panel-linux-amd64")); err != nil {
		t.Fatal("二进制没复制到 dl")
	}

	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(res.Path + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.VerifyWith(pubB64, body, string(sig)); err != nil {
		t.Fatalf("清单验签失败：%v", err)
	}

	var m struct {
		Latest       string           `json:"latest"`
		MinSupported string           `json:"minSupported"`
		Releases     []map[string]any `json:"releases"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Latest != "1.9.1" || len(m.Releases) != 1 {
		t.Fatalf("清单不对：%+v", m)
	}
	if m.Releases[0]["critical"] != true {
		t.Fatal("重大更新标记丢了")
	}
	if m.MinSupported == "" {
		t.Fatal("minSupported 应有默认值")
	}

	// 再发一个版本：新的在前，旧的保留
	if err := os.WriteFile(filepath.Join(dist, "wpqu-panel-darwin-arm64"), []byte("mac"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PackUpdates(UpdatesOptions{DistDir: dist, SiteDir: site, Version: "1.9.2"}, priv); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(res.Path)
	m = struct {
		Latest       string           `json:"latest"`
		MinSupported string           `json:"minSupported"`
		Releases     []map[string]any `json:"releases"`
	}{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Latest != "1.9.2" || len(m.Releases) != 2 || m.Releases[0]["version"] != "1.9.2" {
		t.Fatalf("历史版本未按预期保留：%+v", m.Releases)
	}
}

func TestPackUpdatesNoBinary(t *testing.T) {
	priv, _ := newKey(t)
	dist := t.TempDir()
	site := t.TempDir()
	if _, err := PackUpdates(UpdatesOptions{DistDir: dist, SiteDir: site, Version: "1.0.0"}, priv); err == nil {
		t.Fatal("dist 为空应当报错")
	}
	if _, err := PackUpdates(UpdatesOptions{DistDir: dist, SiteDir: site}, priv); err == nil {
		t.Fatal("缺版本号应当报错")
	}
}
