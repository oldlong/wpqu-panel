package plugins

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sign(t *testing.T, priv ed25519.PrivateKey, data []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))
}

// pluginSource 搭一个假插件源，返回 Manager。
func pluginSource(t *testing.T, pkg []byte, manifest Manifest, badHash bool) *Manager {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pkg)
	manifest.Package = Package{URL: "/plugins/" + manifest.ID + "/1.0.0.zip", Size: int64(len(pkg))}
	if !badHash {
		manifest.Package.SHA256 = hex.EncodeToString(sum[:])
	} else {
		manifest.Package.SHA256 = "deadbeef"
	}
	cat := Catalog{Schema: 1, Updated: "2026-11-16", Plugins: []Manifest{manifest}}
	body, _ := json.Marshal(cat)
	catSig := sign(t, priv, body)
	pkgSig := sign(t, priv, pkg)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/plugins/index.json", func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	mux.HandleFunc("/api/plugins/index.json.sig", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(catSig)) })
	mux.HandleFunc("/plugins/"+manifest.ID+"/1.0.0.zip", func(w http.ResponseWriter, r *http.Request) { w.Write(pkg) })
	mux.HandleFunc("/plugins/"+manifest.ID+"/1.0.0.zip.sig", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(pkgSig)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir())
	m.Base = srv.URL
	m.PublicKey = base64.StdEncoding.EncodeToString(pub)
	return m
}

func sampleManifest() Manifest {
	return Manifest{
		ID: "demo", Version: "1.0.0", Category: "runtime", Tier: "free",
		Name: map[string]string{"zh-CN": "演示", "en": "Demo"},
		Install: &InstallSpec{
			Check: "test -f /opt/demo",
			Steps: []Step{{File: "scripts/install.sh"}},
		},
	}
}

func TestEnsureInstallsPackage(t *testing.T) {
	pkg := makeZip(t, map[string]string{
		"plugin.json":        `{"id":"demo","version":"1.0.0"}`,
		"scripts/install.sh": "#!/bin/sh\necho hi\n",
	})
	m := pluginSource(t, pkg, sampleManifest(), false)

	cat, err := m.FetchCatalog()
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	if len(cat.Plugins) != 1 {
		t.Fatalf("目录条目数 = %d", len(cat.Plugins))
	}
	dir, err := m.Ensure(cat.Plugins[0])
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scripts", "install.sh")); err != nil {
		t.Fatalf("脚本没落地：%v", err)
	}
	if inst, ok := m.Installed()["demo"]; !ok || inst.Version != "1.0.0" {
		t.Fatalf("安装记录不对：%+v", m.Installed())
	}
}

func TestEnsureRejectsHashMismatch(t *testing.T) {
	pkg := makeZip(t, map[string]string{"plugin.json": `{"id":"demo","version":"1.0.0"}`})
	m := pluginSource(t, pkg, sampleManifest(), true)
	cat, err := m.FetchCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(cat.Plugins[0]); err == nil {
		t.Fatal("哈希不符时必须拒绝")
	}
}

func TestEnsureRejectsWrongID(t *testing.T) {
	// 包里写的是别的 id，防止用 A 的清单装 B 的包
	pkg := makeZip(t, map[string]string{"plugin.json": `{"id":"other","version":"1.0.0"}`})
	m := pluginSource(t, pkg, sampleManifest(), false)
	cat, _ := m.FetchCatalog()
	if _, err := m.Ensure(cat.Plugins[0]); err == nil {
		t.Fatal("包内 id 与索引不符时必须拒绝")
	}
}

func TestZipRejectsTraversal(t *testing.T) {
	pkg := makeZip(t, map[string]string{"../evil.sh": "rm -rf /"})
	if err := unzip(pkg, t.TempDir()); err == nil {
		t.Fatal("含 .. 的包必须被拒绝")
	}
}

// ---------------------------------------------------------------- 授权码

func issue(t *testing.T, priv ed25519.PrivateKey, l License) string {
	t.Helper()
	body, _ := json.Marshal(l)
	return "WPQU1." + base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, body))
}

func TestLicenseRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	code := issue(t, priv, License{Product: "demo", Expire: "2099-01-01", Seats: 1, Name: "ACME"})

	lic, err := ParseLicenseWith(base64.StdEncoding.EncodeToString(pub), code)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if lic.Product != "demo" || lic.Name != "ACME" {
		t.Fatalf("载荷不对：%+v", lic)
	}
	if err := lic.Check("demo", time.Now()); err != nil {
		t.Errorf("应该有效：%v", err)
	}
	if err := lic.Check("other", time.Now()); err == nil {
		t.Error("产品不符时应当报错")
	}
}

func TestLicenseExpired(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	code := issue(t, priv, License{Product: "demo", Expire: "2020-01-01"})
	lic, err := ParseLicenseWith(base64.StdEncoding.EncodeToString(pub), code)
	if err != nil {
		t.Fatal(err)
	}
	if err := lic.Check("demo", time.Now()); err == nil {
		t.Fatal("过期码必须报错")
	}
}

func TestLicenseRejectsTamper(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body, _ := json.Marshal(License{Product: "demo", Seats: 99})
	sig := ed25519.Sign(priv, body)
	// 改载荷但保留签名
	body2, _ := json.Marshal(License{Product: "demo", Seats: 999})
	code := "WPQU1." + base64.RawURLEncoding.EncodeToString(body2) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
	if _, err := ParseLicenseWith(base64.StdEncoding.EncodeToString(pub), code); err == nil {
		t.Fatal("篡改过的授权码必须被拒绝")
	}
}

func TestLicenseToleratesWhitespace(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	code := issue(t, priv, License{Product: "demo"})
	// 从邮件复制常见的换行/空格
	messy := " " + code[:10] + "\n" + code[10:] + "  "
	if _, err := ParseLicenseWith(base64.StdEncoding.EncodeToString(pub), messy); err != nil {
		t.Fatalf("应当容忍空白：%v", err)
	}
}
