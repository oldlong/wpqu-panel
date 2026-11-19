package security

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wpqu-panel/internal/signing"
)

func TestComparePkg(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.26.3", "1.26.4", -1},
		{"1.26.3-3+deb13u6", "1.26.3-3+deb13u10", -1}, // 发行版修订单调大
		{"1.26.3-3+deb13u10", "1.26.3-3+deb13u10", 0},
		{"1.28.3", "1.26.3", 1},
		{"1.31.7", "1.26.3", 1},
		{"", "1.0.0", -1},
	}
	for _, c := range cases {
		if got := ComparePkg(c.a, c.b); got != c.want {
			t.Errorf("ComparePkg(%q,%q)=%d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestScanScriptParseApt(t *testing.T) {
	fixes := []Fix{{
		ID:     "nginx-cve",
		Check:  Check{Kind: "apt", Packages: []string{"nginx", "nginx-common"}},
		Action: Action{Steps: []Step{{Kind: "apt", Packages: []string{"nginx"}}}},
	}}
	script := ScanScript(fixes)
	for _, want := range []string{"apt-get update", "nginx-common", "SC\\t", "awk"} {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本里缺少 %q：\n%s", want, script)
		}
	}
	out := "SC\tnginx-cve\tAPT\tnginx\t1.26.3-3+deb13u6\t1.26.3-3+deb13u10\n" +
		"SC\tnginx-cve\tAPT\tnginx-common\t1.26.3-3+deb13u6\t1.26.3-3+deb13u10\n"
	res := ParseScan(fixes, out)["nginx-cve"]
	if !res.Affected || res.Status != "affected" {
		t.Fatalf("应判为受影响：%+v", res)
	}
	if len(res.Updates) != 2 || res.Target != "1.26.3-3+deb13u10" {
		t.Fatalf("升级明细不对：%+v", res)
	}
}

func TestParseScanAptClean(t *testing.T) {
	fixes := []Fix{{ID: "x", Check: Check{Kind: "apt", Packages: []string{"nginx"}}}}
	res := ParseScan(fixes, "")["x"]
	if res.Affected || res.Status != "ok" {
		t.Fatalf("没有输出应当算已最新：%+v", res)
	}
}

func TestParseScanVersion(t *testing.T) {
	fixes := []Fix{{ID: "php", Check: Check{Kind: "version", Command: "php -r ...", Below: "8.4.11"}}}
	res := ParseScan(fixes, "SC\tphp\tVER\t0\t8.4.9\n")["php"]
	if !res.Affected || res.Detail != "8.4.9 → 8.4.11" {
		t.Fatalf("版本判定不对：%+v", res)
	}

	res = ParseScan(fixes, "SC\tphp\tVER\t0\t8.4.13\n")["php"]
	if res.Affected || res.Status != "ok" {
		t.Fatalf("更高版本不应判受影响：%+v", res)
	}

	res = ParseScan(fixes, "SC\tphp\tVER\t1\t\n")["php"]
	if res.Status != "skip" {
		t.Fatalf("读不到版本应为 skip：%+v", res)
	}
}

func TestParseScanCommand(t *testing.T) {
	fixes := []Fix{{ID: "cfg", Check: Check{Kind: "command", Command: "grep -q bad /etc/x"}}}
	if r := ParseScan(fixes, "SC\tcfg\tCMD\t0\n")["cfg"]; !r.Affected {
		t.Fatalf("rc=0 应算受影响：%+v", r)
	}
	if r := ParseScan(fixes, "SC\tcfg\tCMD\t1\n")["cfg"]; r.Affected {
		t.Fatalf("rc=1 不应算受影响：%+v", r)
	}
}

// 未知 id 的行不能污染结果，重复行按 id 归并。
func TestParseScanIgnoresNoise(t *testing.T) {
	fixes := []Fix{{ID: "a", Check: Check{Kind: "apt", Packages: []string{"p"}}}}
	out := "随便一行\nSC\ta\tAPT\tp\t1.0\t2.0\nSC\ta\tAPT\tq\t1.0\t2.0\nSC\tzz\tAPT\tz\t1\t2\n"
	res := ParseScan(fixes, out)
	if len(res) != 1 || len(res["a"].Updates) != 2 {
		t.Fatalf("解析结果不对：%+v", res)
	}
}

func TestUsableFiltersMinPanel(t *testing.T) {
	f := &Feed{Fixes: []Fix{
		{ID: "old", MinPanel: "1.0.0", Action: Action{Steps: []Step{{Kind: "sh", Script: "true"}}}},
		{ID: "future", MinPanel: "9.9.9", Action: Action{Steps: []Step{{Kind: "sh", Script: "true"}}}},
		{ID: "nosteps"},
	}}
	got := f.Usable("1.10.0")
	if len(got) != 1 || got[0].ID != "old" {
		t.Fatalf("筛选结果不对：%+v", got)
	}
}

func TestFixScript(t *testing.T) {
	f := Fix{Action: Action{Steps: []Step{
		{Kind: "apt", Packages: []string{"nginx"}},
		{Kind: "sh", Label: "重载 nginx", Script: "nginx -t && systemctl reload nginx"},
	}}}
	s := f.Script()
	for _, want := range []string{"set -e", "apt-get update", "apt-get install -y --only-upgrade 'nginx'", "重载 nginx", "systemctl reload"} {
		if !strings.Contains(s, want) {
			t.Fatalf("脚本缺少 %q：\n%s", want, s)
		}
	}
	// update 必须在 install 之前，否则索引还是旧的
	if strings.Index(s, "apt-get update") > strings.Index(s, "apt-get install") {
		t.Fatalf("刷新索引应在安装之前：\n%s", s)
	}
	if strings.Contains(s, "| bash") {
		t.Fatal("脚本不应包含管道执行")
	}
}

func TestFixScriptInstall(t *testing.T) {
	f := Fix{Action: Action{Steps: []Step{{Kind: "apt", Packages: []string{"certbot"}, Install: true}}}}
	if !strings.Contains(f.Script(), "apt-get install -y 'certbot'") {
		t.Fatalf("install=true 不应加 --only-upgrade：\n%s", f.Script())
	}
}

// 纯 sh 的修复（比如只重载服务）不该出现 apt-get update
func TestFixScriptNoAptStepSkipsUpdate(t *testing.T) {
	f := Fix{Action: Action{Steps: []Step{{Kind: "sh", Script: "systemctl reload nginx"}}}}
	if strings.Contains(f.Script(), "apt-get update") {
		t.Fatalf("没有 apt 步骤时不该刷索引：\n%s", f.Script())
	}
}

// ---------------------------------------------------------------- 清单验签

func newTestFeedServer(t *testing.T, body []byte, sig string) (*Client, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/security.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	mux.HandleFunc("/api/security.json.sig", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sig))
	})
	srv := httptest.NewServer(mux)
	return &Client{Base: srv.URL, Current: "1.10.0", HTTP: srv.Client()}, srv.Close
}

func TestFeedAcceptsSignedBody(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	body, _ := json.Marshal(Feed{
		Schema: 1,
		Fixes: []Fix{{
			ID: "nginx", Level: "critical",
			Action: Action{Steps: []Step{{Kind: "apt", Packages: []string{"nginx"}}}},
		}},
	})
	c, closeFn := newTestFeedServer(t, body, signing.Sign(priv, body)+"\n")
	defer closeFn()
	c.PublicKey = base64.StdEncoding.EncodeToString(pub)

	f, err := c.Feed(true)
	if err != nil {
		t.Fatalf("验签应通过：%v", err)
	}
	if len(f.Fixes) != 1 || f.Fixes[0].ID != "nginx" {
		t.Fatalf("清单内容不对：%+v", f)
	}
}

func TestFeedRejectsTamperedBody(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	body := []byte(`{"schema":1,"fixes":[{"id":"evil","action":{"steps":[{"kind":"sh","script":"rm -rf /"}]}}]}`)
	sig := signing.Sign(priv, []byte(`{"schema":1,"fixes":[]}`)) // 签的是别的内容
	c, closeFn := newTestFeedServer(t, body, sig+"\n")
	defer closeFn()
	c.PublicKey = base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))

	if _, err := c.Feed(true); err == nil {
		t.Fatal("被篡改的清单必须拒绝")
	}
}

func TestFeed404IsEmptyNotError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := &Client{Base: srv.URL, Current: "1.10.0", HTTP: srv.Client()}
	f, err := c.Feed(true)
	if err != nil {
		t.Fatalf("404 应返回空清单：%v", err)
	}
	if len(f.Fixes) != 0 {
		t.Fatalf("应为空：%+v", f)
	}
}

func TestFeedMissingSigRejected(t *testing.T) {
	body := []byte(`{"schema":1,"fixes":[]}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/security.json", func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{Base: srv.URL, Current: "1.10.0", HTTP: srv.Client()}
	if _, err := c.Feed(true); err == nil {
		t.Fatal("缺签名必须拒绝")
	}
}

func TestSafeID(t *testing.T) {
	if safeID("nginx-cve") != "nginx-cve" {
		t.Fatal("正常 id 不该被改")
	}
	if safeID("a'b") != "invalid_id" {
		t.Fatal("异常字符要替换")
	}
}
