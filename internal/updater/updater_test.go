package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
)

// fakeSource 搭一个假更新源：清单 + 签名 + 二进制。
func fakeSource(t *testing.T, mutate func(*Manifest), tamper bool) (*httptest.Server, string, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bin := []byte("#!/bin/sh\necho wpqu-panel\n")
	sum := sha256.Sum256(bin)

	m := Manifest{
		Schema:  1,
		Channel: "stable",
		Latest:  "1.9.1",
		Releases: []Release{{
			Version: "1.9.1",
			Date:    "2026-11-20",
			Notes:   map[string][]string{"zh-CN": {"修好了 Apache 队列"}, "en": {"Fixed Apache queue"}},
			Files: []Package{{
				OS: runtime.GOOS, Arch: runtime.GOARCH, URL: "/dl/bin",
				SHA256: hex.EncodeToString(sum[:]), Size: int64(len(bin)),
			}},
		}},
	}
	if mutate != nil {
		mutate(&m)
	}
	body, _ := json.Marshal(m)
	sig := ed25519.Sign(priv, body)
	if tamper {
		// 改一个字节，模拟中间人篡改清单
		body = append(body, ' ')
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/updates.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	mux.HandleFunc("/api/updates.json.sig", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(base64.StdEncoding.EncodeToString(sig)))
	})
	mux.HandleFunc("/dl/bin", func(w http.ResponseWriter, r *http.Request) {
		w.Write(bin)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, base64.StdEncoding.EncodeToString(pub), bin
}

func TestCheckFindsUpdate(t *testing.T) {
	srv, pub, _ := fakeSource(t, nil, false)
	c := New(t.TempDir(), "1.9.0")
	c.Base, c.PublicKey = srv.URL, pub

	res, err := c.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Available || res.Latest != "1.9.1" {
		t.Fatalf("应发现 1.9.1：%+v", res)
	}
	if res.SHA256 == "" || res.DownloadURL == "" {
		t.Fatalf("缺少下载信息：%+v", res)
	}
	if len(res.Notes["zh-CN"]) == 0 {
		t.Error("更新说明没带过来")
	}
}

func TestCheckAlreadyLatest(t *testing.T) {
	srv, pub, _ := fakeSource(t, nil, false)
	c := New(t.TempDir(), "1.9.1")
	c.Base, c.PublicKey = srv.URL, pub
	res, err := c.Check()
	if err != nil {
		t.Fatal(err)
	}
	if res.Available {
		t.Fatalf("已是最新却提示更新：%+v", res)
	}
}

func TestCheckRejectsTamperedManifest(t *testing.T) {
	srv, pub, _ := fakeSource(t, nil, true)
	c := New(t.TempDir(), "1.9.0")
	c.Base, c.PublicKey = srv.URL, pub
	if _, err := c.Check(); err == nil {
		t.Fatal("篡改过的清单必须被拒绝")
	}
}

func TestCheckNoPackageForPlatform(t *testing.T) {
	srv, pub, _ := fakeSource(t, func(m *Manifest) {
		m.Releases[0].Files[0].OS = "plan9"
	}, false)
	c := New(t.TempDir(), "1.9.0")
	c.Base, c.PublicKey = srv.URL, pub
	res, err := c.Check()
	if err != nil {
		t.Fatal(err)
	}
	if res.Available {
		t.Fatalf("没有本平台包时不应标为可升级：%+v", res)
	}
	if res.Skipped == "" {
		t.Error("应说明为什么不升级")
	}
}

func TestStageVerifiesAndWrites(t *testing.T) {
	srv, pub, bin := fakeSource(t, nil, false)
	c := New(t.TempDir(), "1.9.0")
	c.Base, c.PublicKey = srv.URL, pub

	res, err := c.Check()
	if err != nil {
		t.Fatal(err)
	}
	staged, err := c.Stage(res, nil)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(bin) {
		t.Fatalf("落地内容不一致：%q", got)
	}
}

func TestStageRejectsBadHash(t *testing.T) {
	srv, pub, _ := fakeSource(t, func(m *Manifest) {
		m.Releases[0].Files[0].SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	}, false)
	c := New(t.TempDir(), "1.9.0")
	c.Base, c.PublicKey = srv.URL, pub
	res, err := c.Check()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stage(res, nil); err == nil {
		t.Fatal("SHA256 不符时必须拒绝落地")
	}
}
