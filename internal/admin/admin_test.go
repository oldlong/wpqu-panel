package admin

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wpqu-panel/internal/licensing"
	"wpqu-panel/internal/signing"
)

const testPassword = "test-password-123"

// harness 搭一套最小可用的目录结构：两个插件、一个站点、两个私钥。
type harness struct {
	srv  *httptest.Server
	app  *Server
	cfg  Config
	keys struct{ release, license ed25519.PrivateKey }
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		SiteDir:      filepath.Join(root, "site"),
		SrcDir:       filepath.Join(root, "plugins-src"),
		DistDir:      filepath.Join(root, "dist"),
		KeysDir:      filepath.Join(root, "keys"),
		DataDir:      filepath.Join(root, "data"),
		SessionHours: 1,
	}
	for _, d := range []string{cfg.SiteDir, cfg.SrcDir, cfg.DistDir, cfg.KeysDir, cfg.DataDir,
		filepath.Join(cfg.SiteDir, "api", "plugins"), filepath.Join(cfg.SiteDir, "assets")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	h := &harness{cfg: cfg}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h.keys.release = priv
	if err := os.WriteFile(filepath.Join(cfg.KeysDir, "release.key"),
		[]byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = pub
	lpub, lpriv, _ := ed25519.GenerateKey(rand.Reader)
	h.keys.license = lpriv
	if err := os.WriteFile(filepath.Join(cfg.KeysDir, "license.key"),
		[]byte(base64.StdEncoding.EncodeToString(lpriv)), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = lpub

	writePluginDir(t, cfg.SrcDir, "alpha", "1.0.0", "free", 0)
	writePluginDir(t, cfg.SrcDir, "beta", "2.0.0", "paid", 199)

	if err := os.WriteFile(filepath.Join(cfg.SiteDir, "index.html"),
		[]byte("<html><body>hello</body></html>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.SiteDir, ".keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.SiteDir, ".keys", "release.key"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 二进制文件不该出现在可编辑列表里
	if err := os.WriteFile(filepath.Join(cfg.SiteDir, "assets", "logo.png"), []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := NewAuth("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Save(filepath.Join(cfg.DataDir, "auth.json")); err != nil {
		t.Fatal(err)
	}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
	h.app = s
	h.srv = httptest.NewServer(s)
	t.Cleanup(h.srv.Close)
	return h
}

func writePluginDir(t *testing.T, src, id, ver, tier string, price int) {
	t.Helper()
	dir := filepath.Join(src, id)
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	mf := map[string]any{
		"id": id, "version": ver, "category": "runtime", "tier": tier,
		"name":     map[string]string{"zh-CN": "插件" + id, "en": id},
		"requires": map[string]any{"os": []string{"linux"}, "minPanel": "1.9.0"},
		"install":  map[string]any{"check": "true", "steps": []any{}},
	}
	if price > 0 {
		mf["priceCny"] = price
	}
	raw, _ := json.MarshalIndent(mf, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "install.sh"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// client 返回一个带 cookie 的客户端，方便串起登录后的流程。
func (h *harness) client(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func (h *harness) do(t *testing.T, c *http.Client, method, path string, body any, mutate bool) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate {
		req.Header.Set("X-Admin", "1")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func (h *harness) login(t *testing.T, c *http.Client) {
	t.Helper()
	code, body := h.do(t, c, "POST", "/api/login", map[string]string{"password": testPassword}, false)
	if code != 200 {
		t.Fatalf("登录失败：%d %v", code, body)
	}
}

func TestUnauthenticatedIsRejected(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	for _, p := range []string{"/api/overview", "/api/plugins", "/api/licenses", "/api/site/tree"} {
		if code, _ := h.do(t, c, "GET", p, nil, false); code != 401 {
			t.Fatalf("%s 未登录应返回 401，实际 %d", p, code)
		}
	}
}

// 双击第二个实例时，新进程靠 /api/ping 认出旧实例，所以它必须免登录。
func TestPingNeedsNoSession(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	code, body := h.do(t, c, "GET", "/api/ping", nil, false)
	if code != 200 {
		t.Fatalf("ping 应免登录可访问，实际 %d %v", code, body)
	}
	if body["app"] != "wpquadmin" {
		t.Fatalf("ping 应标明身份，实际 %v", body)
	}
	// 带上 ping 之后，其它接口依然要登录
	if code, _ := h.do(t, c, "GET", "/api/overview", nil, false); code != 401 {
		t.Fatalf("ping 不该顺带放行其它接口，实际 %d", code)
	}
}

// 双击时工作目录是 exe 目录（dist/admin），配置里的 ../site 会指到 dist/site。
// Locate 必须能从 exe 位置往上找到真正的 site/。
func TestLocateFromExeDir(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "site")
	src := filepath.Join(root, "plugins-src")
	dist := filepath.Join(root, "dist")
	exeDir := filepath.Join(dist, "admin")
	for _, d := range []string{
		filepath.Join(site, "api", "plugins"), filepath.Join(site, "plugins"),
		filepath.Join(src, "alpha"), dist, exeDir, filepath.Join(site, ".keys"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "api", "plugins", "index.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "alpha", "plugin.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "wpqu-panel-linux-amd64"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 模拟默认配置按 exeDir 解析出来的错误路径
	cfg := Config{
		SiteDir: filepath.Join(dist, "site"),
		SrcDir:  filepath.Join(dist, "plugins-src"),
		DistDir: filepath.Join(dist, "dist"),
		KeysDir: filepath.Join(dist, "site", ".keys"),
		DataDir: filepath.Join(exeDir, "admin-data"),
	}
	notes := cfg.Locate(exeDir, 0)
	if cfg.SiteDir != site {
		t.Fatalf("站点应纠正为 %s，实际 %s（notes=%v）", site, cfg.SiteDir, notes)
	}
	if cfg.SrcDir != src {
		t.Fatalf("插件源应纠正为 %s，实际 %s", src, cfg.SrcDir)
	}
	if cfg.DistDir != dist {
		t.Fatalf("构建目录应纠正为 %s，实际 %s", dist, cfg.DistDir)
	}
	if cfg.KeysDir != filepath.Join(site, ".keys") {
		t.Fatalf("私钥目录应跟着站点走，实际 %s", cfg.KeysDir)
	}
	if len(notes) == 0 {
		t.Fatal("纠正过路径就应该有说明，否则用户不知道实际用的是哪个目录")
	}

	// 用户写死的正确路径不能被改
	good := Config{SiteDir: site, SrcDir: src, DistDir: dist, KeysDir: filepath.Join(site, ".keys")}
	if n := good.Locate(exeDir, 0); len(n) != 0 {
		t.Fatalf("路径都对时不应有改动，实际 %v", n)
	}
}

// exe 直接放在仓库根（site 的同级）时也要认得出来。
func TestFindSiteDirAtRepoRoot(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "site")
	if err := os.MkdirAll(filepath.Join(site, "api", "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "api", "plugins", "index.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := FindSiteDir(root)
	if !ok || got != site {
		t.Fatalf("应从仓库根认出 %s，实际 %q ok=%v", site, got, ok)
	}
	if _, ok := FindSiteDir(t.TempDir()); ok {
		t.Fatal("空目录不该被认成分发站")
	}
}

// 命令行显式 `-site` 指定的目录优先级最高：哪怕它看起来不像分发站，
// 也不能被探测结果悄悄换掉。
func TestLocateKeepsExplicit(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "site")
	if err := os.MkdirAll(filepath.Join(site, "api", "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "api", "plugins", "index.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 用户指定的另一个目录：暂时还没有索引文件，也不该被替换
	mine := filepath.Join(root, "my-feed")
	if err := os.MkdirAll(mine, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{SiteDir: mine}
	cfg.Locate(root, KeepSite)
	if cfg.SiteDir != mine {
		t.Fatalf("显式指定的站点目录被改成了 %s", cfg.SiteDir)
	}
}

// 手改配置时写 `C:/Users/...` 或者多打几个斜杠都很常见，必须归一化，
// 否则 Windows 上拼接出来的路径会变成 `C://Users//...`。
func TestLoadConfigNormalizesSlashes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "admin.json")
	body := `{"siteDir":"C:/a/b","keysDir":"C:\\a\\b\\.keys","dataDir":"../data","listen":"127.0.0.1:7790"}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, created, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("文件存在时不该报 created")
	}
	if strings.Contains(cfg.SiteDir, "//") {
		t.Fatalf("站点路径没归一化：%q", cfg.SiteDir)
	}
	if want := filepath.Clean(cfg.SiteDir); cfg.SiteDir != want {
		t.Fatalf("站点路径应为 %q，实际 %q", want, cfg.SiteDir)
	}
	// 相对路径按配置文件所在目录解析（`../data` 就是上一级的 data）
	if want := filepath.Join(filepath.Dir(dir), "data"); cfg.DataDir != want {
		t.Fatalf("相对路径应相对配置文件目录，期望 %q，实际 %q", want, cfg.DataDir)
	}
}

// 手改配置把某项写成空串时按默认值补齐，而不是留个空路径去 mkdir。
func TestLoadConfigFillsEmptyFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "admin.json")
	body := `{"siteDir":"","keysDir":"","dataDir":"","sessionHours":0}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir == "" || cfg.KeysDir == "" || cfg.SiteDir == "" {
		t.Fatalf("空字段应补默认值，实际 %+v", cfg)
	}
	if cfg.SessionHours <= 0 {
		t.Fatalf("会话时长应补成默认值，实际 %d", cfg.SessionHours)
	}
	if !filepath.IsAbs(cfg.DataDir) {
		t.Fatalf("补完默认值后也应是绝对路径，实际 %q", cfg.DataDir)
	}
}

func TestLoginFlow(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)

	code, _ := h.do(t, c, "POST", "/api/login", map[string]string{"password": "wrong"}, false)
	if code != 401 {
		t.Fatalf("错误口令应 401，实际 %d", code)
	}

	h.login(t, c)
	code, body := h.do(t, c, "GET", "/api/overview", nil, false)
	if code != 200 {
		t.Fatalf("登录后应可访问：%d %v", code, body)
	}
	if body["keys"] == nil || body["plugins"] == nil {
		t.Fatalf("概览缺字段：%v", body)
	}

	// 退出后失效
	if code, _ := h.do(t, c, "POST", "/api/logout", map[string]string{}, true); code != 200 {
		t.Fatalf("退出失败：%d", code)
	}
	if code, _ := h.do(t, c, "GET", "/api/overview", nil, false); code != 401 {
		t.Fatalf("退出后应 401，实际 %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	var last int
	for i := 0; i < 7; i++ {
		last, _ = h.do(t, c, "POST", "/api/login", map[string]string{"password": "nope"}, false)
		if last == 429 {
			break
		}
	}
	if last != 429 {
		t.Fatalf("连续失败后应触发限流，最后状态 %d", last)
	}
	// 限流期间即使口令正确也进不来
	code, _ := h.do(t, c, "POST", "/api/login", map[string]string{"password": testPassword}, false)
	if code != 429 {
		t.Fatalf("限流期间应仍返回 429，实际 %d", code)
	}
}

func TestCSRFHeaderRequired(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	h.login(t, c)

	// 没有 X-Admin 头的写操作一律拒绝
	code, _ := h.do(t, c, "PUT", "/api/site/file",
		map[string]string{"root": "site", "path": "index.html", "content": "x"}, false)
	if code != 403 {
		t.Fatalf("缺 X-Admin 应 403，实际 %d", code)
	}

	// 跨站 Origin 也拒绝
	req, _ := http.NewRequest("PUT", h.srv.URL+"/api/site/file",
		strings.NewReader(`{"root":"site","path":"index.html","content":"x"}`))
	req.Header.Set("X-Admin", "1")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("跨站请求应 403，实际 %d", resp.StatusCode)
	}
}

func TestIssueLicenseAndVerify(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	h.login(t, c)

	code, body := h.do(t, c, "POST", "/api/licenses", map[string]any{
		"product": "beta", "expire": "2099-12-31", "seats": 3,
		"name": "某某科技", "note": "微信 199", "count": 2,
	}, true)
	if code != 200 {
		t.Fatalf("发码失败：%d %v", code, body)
	}
	codes, _ := body["codes"].([]any)
	if len(codes) != 2 {
		t.Fatalf("应签发 2 张，实际 %d", len(codes))
	}

	// 用私钥对应的公钥验一遍，证明码本身是有效的
	pub, err := signing.PublicKeyOf(filepath.Join(h.cfg.KeysDir, "license.key"))
	if err != nil {
		t.Fatal(err)
	}
	lic, err := licensing.ParseWith(pub, codes[0].(string))
	if err != nil {
		t.Fatalf("签出来的码验不过：%v", err)
	}
	if lic.Product != "beta" || lic.Seats != 3 || lic.Name != "某某科技" {
		t.Fatalf("载荷不对：%+v", lic)
	}

	// 台账里有两条
	code, list := h.do(t, c, "GET", "/api/licenses", nil, false)
	if code != 200 {
		t.Fatalf("读台账失败：%d", code)
	}
	rows, _ := list["licenses"].([]any)
	if len(rows) != 2 {
		t.Fatalf("台账应有 2 条，实际 %d", len(rows))
	}
	summary, _ := list["summary"].(map[string]any)
	if summary["active"] != float64(2) {
		t.Fatalf("活跃数不对：%v", summary)
	}

	// 服务端校验接口：签名用的是本机私钥，所以能验过；
	// 但测试用的密钥与编译进面板的常量不同，必须明确提示面板会拒绝。
	code, v := h.do(t, c, "POST", "/api/licenses/verify", map[string]string{"code": codes[0].(string)}, true)
	if code != 200 || v["ok"] != true {
		t.Fatalf("校验失败：%d %v", code, v)
	}
	if v["panelKeyMatch"] != false || v["warning"] == nil {
		t.Fatalf("测试密钥与面板常量不同，应给出不匹配警告：%v", v)
	}

	// 别人签的码必须验不过，但载荷仍要能解出来供排查
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = otherPub
	foreign, _, err := licensing.Issue(otherPriv, licensing.License{Product: "beta", Name: "伪造"})
	if err != nil {
		t.Fatal(err)
	}
	code, v = h.do(t, c, "POST", "/api/licenses/verify", map[string]string{"code": foreign}, true)
	if code != 200 || v["ok"] == true {
		t.Fatalf("外来码不该通过：%v", v)
	}
	if v["decoded"] == nil {
		t.Fatalf("验签失败时仍应能展示载荷供排查：%v", v)
	}
}

func TestIssueRejectsMissingFields(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	h.login(t, c)

	cases := []map[string]any{
		{"product": "beta", "name": ""},                          // 缺客户名
		{"product": "nope", "name": "X"},                         // 插件不存在
		{"product": "beta", "name": "X", "expire": "2099/01/01"}, // 日期格式
	}
	for _, body := range cases {
		if code, _ := h.do(t, c, "POST", "/api/licenses", body, true); code != 400 {
			t.Fatalf("%v 应被拒绝，实际 %d", body, code)
		}
	}
}

func TestVoidLicense(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	h.login(t, c)

	_, body := h.do(t, c, "POST", "/api/licenses", map[string]any{"product": "beta", "name": "张三"}, true)
	entries, _ := body["entries"].([]any)
	id := entries[0].(map[string]any)["id"].(string)

	code, res := h.do(t, c, "POST", "/api/licenses/"+id+"/void", map[string]string{"reason": "退款"}, true)
	if code != 200 {
		t.Fatalf("作废失败：%d %v", code, res)
	}
	if msg, _ := res["notice"].(string); !strings.Contains(msg, "不会让码失效") {
		t.Fatalf("作废提示应说明不可吊销：%q", msg)
	}

	_, list := h.do(t, c, "GET", "/api/licenses", nil, false)
	rows, _ := list["licenses"].([]any)
	row := rows[0].(map[string]any)
	if row["status"] != "已作废" || row["void"] != true {
		t.Fatalf("台账未标记作废：%v", row)
	}
	if code, _ := h.do(t, c, "POST", "/api/licenses/NOPE/void", map[string]string{}, true); code != 404 {
		t.Fatalf("不存在的 id 应 404，实际 %d", code)
	}
}

func TestPluginsListAndBuild(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	h.login(t, c)

	code, body := h.do(t, c, "GET", "/api/plugins", nil, false)
	if code != 200 {
		t.Fatalf("读插件失败：%d", code)
	}
	plugins, _ := body["plugins"].([]any)
	if len(plugins) != 2 {
		t.Fatalf("应有 2 个插件，实际 %d", len(plugins))
	}
	first := plugins[0].(map[string]any)
	if first["drift"] != "unpublished" {
		t.Fatalf("没发布过的插件 drift 应为 unpublished：%v", first["drift"])
	}

	// 演练不落盘
	if code, _ := h.do(t, c, "POST", "/api/market/build", map[string]any{"dryRun": true}, true); code != 200 {
		t.Fatalf("演练失败：%d", code)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.SiteDir, "plugins")); !os.IsNotExist(err) {
		t.Fatal("演练不应写包")
	}

	// 正式发布后索引与签名都在
	code, res := h.do(t, c, "POST", "/api/market/build", map[string]any{"dryRun": false}, true)
	if code != 200 {
		t.Fatalf("发布失败：%d %v", code, res)
	}
	indexPath := filepath.Join(h.cfg.SiteDir, "api", "plugins", "index.json")
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(indexPath + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.VerifyWith(signing.PublicKeyB64(h.keys.release), raw, string(sig)); err != nil {
		t.Fatalf("索引签名验不过：%v", err)
	}

	// 发布后列表应显示已同步
	_, body = h.do(t, c, "GET", "/api/plugins", nil, false)
	for _, p := range body["plugins"].([]any) {
		if d := p.(map[string]any)["drift"]; d != "" {
			t.Fatalf("发布后不该再有 drift：%v", d)
		}
	}
}

func TestManifestEditAndVersionBump(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	h.login(t, c)

	code, detail := h.do(t, c, "GET", "/api/plugins/alpha", nil, false)
	if code != 200 {
		t.Fatalf("读详情失败：%d", code)
	}
	raw, _ := detail["raw"].(string)
	if !strings.Contains(raw, `"alpha"`) {
		t.Fatalf("详情里应有清单原文：%s", raw)
	}

	// 改价格
	mf := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &mf)
	mf["tier"] = "paid"
	mf["priceCny"] = 99
	updated, _ := json.MarshalIndent(mf, "", "  ")
	code, res := h.do(t, c, "PUT", "/api/plugins/alpha/manifest", map[string]string{"json": string(updated)}, true)
	if code != 200 {
		t.Fatalf("保存清单失败：%d %v", code, res)
	}

	// 版本进位
	code, res = h.do(t, c, "POST", "/api/plugins/alpha/version", map[string]string{"mode": "minor"}, true)
	if code != 200 {
		t.Fatalf("进位失败：%d %v", code, res)
	}
	if res["version"] != "1.1.0" {
		t.Fatalf("次版本应为 1.1.0，实际 %v", res["version"])
	}

	// 版本号不合法要被拒
	if code, _ := h.do(t, c, "PUT", "/api/plugins/alpha/manifest",
		map[string]string{"json": `{"id":"alpha","version":"abc","name":"x"}`}, true); code != 400 {
		t.Fatalf("非法版本号应 400，实际 %d", code)
	}
	// id 与目录名不一致要被拒
	if code, _ := h.do(t, c, "PUT", "/api/plugins/alpha/manifest",
		map[string]string{"json": `{"id":"beta","version":"9.9.9","name":"x"}`}, true); code != 400 {
		t.Fatalf("id 不符应 400，实际 %d", code)
	}
}

func TestSiteFileEditBackupRestore(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	h.login(t, c)

	code, body := h.do(t, c, "GET", "/api/site/tree?root=site", nil, false)
	if code != 200 {
		t.Fatalf("读目录失败：%d", code)
	}
	files, _ := body["files"].([]any)
	var paths []string
	for _, f := range files {
		paths = append(paths, f.(map[string]any)["path"].(string))
	}
	if !strings.Contains(strings.Join(paths, ","), "index.html") {
		t.Fatalf("树里应有 index.html：%v", paths)
	}
	for _, p := range paths {
		if strings.Contains(p, ".key") || strings.HasSuffix(p, ".png") {
			t.Fatalf("不该出现 %s", p)
		}
	}

	// 写入并产生备份
	code, res := h.do(t, c, "PUT", "/api/site/file", map[string]string{
		"root": "site", "path": "index.html", "content": "<html>v2</html>\n",
	}, true)
	if code != 200 {
		t.Fatalf("保存失败：%d %v", code, res)
	}
	if res["backup"] == nil || res["backup"] == "" {
		t.Fatalf("应产生备份：%v", res)
	}

	// 再存一次同样的内容：不该再留备份
	code, res2 := h.do(t, c, "PUT", "/api/site/file", map[string]string{
		"root": "site", "path": "index.html", "content": "<html>v2</html>\n",
	}, true)
	if code != 200 || res2["unchanged"] != true {
		t.Fatalf("相同内容应跳过：%d %v", code, res2)
	}

	// 读回
	_, body = h.do(t, c, "GET", "/api/site/file?root=site&path=index.html", nil, false)
	if body["content"] != "<html>v2</html>\n" {
		t.Fatalf("内容不对：%v", body["content"])
	}
	backups, _ := body["backups"].([]any)
	if len(backups) != 1 {
		t.Fatalf("应有 1 个备份，实际 %d", len(backups))
	}

	// 还原
	name := backups[0].(map[string]any)["name"].(string)
	code, res = h.do(t, c, "POST", "/api/site/restore",
		map[string]string{"root": "site", "path": "index.html", "backup": name}, true)
	if code != 200 {
		t.Fatalf("还原失败：%d %v", code, res)
	}
	if res["content"] != "<html><body>hello</body></html>\n" {
		t.Fatalf("还原内容不对：%v", res["content"])
	}
}

func TestSitePathTraversalBlocked(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	h.login(t, c)

	bad := []string{
		"../plugins-src/alpha/plugin.json",
		"../../etc/passwd",
		".keys/release.key",
		"assets/../../outside.txt",
		"/etc/passwd",
	}
	for _, p := range bad {
		code, body := h.do(t, c, "GET", "/api/site/file?root=site&path="+urlQueryEscape(p), nil, false)
		if code == 200 {
			t.Fatalf("路径 %q 不该可读：%v", p, body)
		}
	}
	// 二进制文件也不给读
	if code, _ := h.do(t, c, "GET", "/api/site/file?root=site&path=assets/logo.png", nil, false); code == 200 {
		t.Fatal("png 不该可读")
	}
	// api/ 下是签名生成物，手改会把签名弄废
	for _, p := range []string{"api/plugins/index.json", "api/updates.json"} {
		if code, _ := h.do(t, c, "GET", "/api/site/file?root=site&path="+p, nil, false); code != 400 {
			t.Fatalf("%s 不该可编辑，实际 %d", p, code)
		}
	}
	// 未知根目录
	if code, _ := h.do(t, c, "GET", "/api/site/tree?root=etc", nil, false); code != 400 {
		t.Fatal("未知根目录应 400")
	}
}

func urlQueryEscape(s string) string {
	r := strings.NewReplacer("&", "%26", "#", "%23", "?", "%3F", "+", "%2B", " ", "%20")
	return r.Replace(s)
}

func TestUpdatesPublish(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	h.login(t, c)

	for _, name := range []string{"wpqu-panel-linux-amd64", "wpqu-panel-windows-amd64.exe"} {
		if err := os.WriteFile(filepath.Join(h.cfg.DistDir, name), []byte("bin-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	code, info := h.do(t, c, "GET", "/api/updates", nil, false)
	if code != 200 {
		t.Fatalf("读更新信息失败：%d", code)
	}
	bins, _ := info["distFiles"].([]any)
	if len(bins) != 2 {
		t.Fatalf("应识别 2 个二进制，实际 %d", len(bins))
	}

	// 没有说明不许发
	if code, _ := h.do(t, c, "POST", "/api/updates/publish",
		map[string]any{"version": "1.9.1"}, true); code != 400 {
		t.Fatalf("缺说明应 400，实际 %d", code)
	}

	code, res := h.do(t, c, "POST", "/api/updates/publish", map[string]any{
		"version": "1.9.1", "critical": true,
		"notesZh": "- 修了 A\n- 加了 B", "notesEn": "Fixed A",
	}, true)
	if code != 200 {
		t.Fatalf("发布失败：%d %v", code, res)
	}
	if res["platforms"] != float64(2) {
		t.Fatalf("平台数不对：%v", res)
	}

	raw, err := os.ReadFile(filepath.Join(h.cfg.SiteDir, "api", "updates.json"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(filepath.Join(h.cfg.SiteDir, "api", "updates.json.sig"))
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.VerifyWith(signing.PublicKeyB64(h.keys.release), raw, string(sig)); err != nil {
		t.Fatalf("清单验签失败：%v", err)
	}
	var m struct {
		Latest   string           `json:"latest"`
		Releases []map[string]any `json:"releases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Latest != "1.9.1" || m.Releases[0]["critical"] != true {
		t.Fatalf("清单内容不对：%+v", m)
	}
	notes := m.Releases[0]["notes"].(map[string]any)
	if arr, _ := notes["zh-CN"].([]any); len(arr) != 2 {
		t.Fatalf("中文说明应拆成 2 行：%v", notes)
	}
}

func TestAuditRecorded(t *testing.T) {
	h := newHarness(t)
	c := h.client(t)
	h.login(t, c)
	h.do(t, c, "POST", "/api/licenses", map[string]any{"product": "alpha", "name": "审计"}, true)

	code, body := h.do(t, c, "GET", "/api/audit?limit=50", nil, false)
	if code != 200 {
		t.Fatalf("读审计失败：%d", code)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) < 2 {
		t.Fatalf("至少应有登录与发码两条：%d", len(entries))
	}
	var hasIssue bool
	for _, e := range entries {
		if e.(map[string]any)["action"] == "license.issue" {
			hasIssue = true
		}
	}
	if !hasIssue {
		t.Fatal("发码没有留痕")
	}
}

func TestSafeRel(t *testing.T) {
	// 这些必须拒绝：能跳出根目录，或指向私钥
	bad := []string{"", "../x", "a/../../b", ".keys/k", "x/y.key"}
	for _, p := range bad {
		if got, err := safeRel(p); err == nil {
			t.Fatalf("%q 应被拒，得到 %q", p, got)
		}
	}
	// 开头的斜杠会被去掉，结果仍然落在根目录内，属于可接受
	good := map[string]string{
		"index.html":        "index.html",
		"assets/site.css":   "assets/site.css",
		"./docs/index.html": "docs/index.html",
		"/etc/passwd":       "etc/passwd",
	}
	for in, want := range good {
		got, err := safeRel(in)
		if err != nil || got != want {
			t.Fatalf("%q 应为 %q，实际 %q (%v)", in, want, got, err)
		}
	}
	// 绝对值也不能真的读到 /etc/passwd，resolve 会把它拼到站点目录下
	h := newHarness(t)
	if _, abs, err := h.app.resolve("site", "/etc/passwd"); err == nil {
		t.Fatalf("应当因为扩展名不合适被拒：%s", abs)
	}
}

func TestBumpVersion(t *testing.T) {
	cases := []struct{ cur, mode, want string }{
		{"1.0.0", "patch", "1.0.1"},
		{"1.0.9", "minor", "1.1.0"},
		{"1.9.9", "major", "2.0.0"},
		{"2.3", "patch", "2.3.1"},
	}
	for _, c := range cases {
		got, err := bumpVersion(c.cur, c.mode)
		if err != nil || got != c.want {
			t.Fatalf("%s/%s 应为 %s，实际 %s (%v)", c.cur, c.mode, c.want, got, err)
		}
	}
	if _, err := bumpVersion("abc", "patch"); err == nil {
		t.Fatal("非数字版本号应报错")
	}
}

func TestSplitNotes(t *testing.T) {
	got := splitNotes("- 第一行\n\n  * 第二行\n第三行")
	want := []string{"第一行", "第二行", "第三行"}
	if len(got) != len(want) {
		t.Fatalf("应为 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 行应为 %q，实际 %q", i, want[i], got[i])
		}
	}
}

func TestAuthRoundTrip(t *testing.T) {
	a, err := NewAuth("admin", "abcdefgh")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Verify("abcdefgh") {
		t.Fatal("正确口令应通过")
	}
	if a.Verify("abcdefghi") || a.Verify("") {
		t.Fatal("错误口令不该通过")
	}
	if _, err := NewAuth("admin", "short"); err == nil {
		t.Fatal("短口令应被拒绝")
	}
}

func TestSessionsExpire(t *testing.T) {
	s := NewSessions(50 * time.Millisecond)
	token, _ := s.New()
	if !s.Valid(token) {
		t.Fatal("刚签发的会话应有效")
	}
	time.Sleep(70 * time.Millisecond)
	if s.Valid(token) {
		t.Fatal("过期后应失效")
	}
	t2, _ := s.New()
	s.Drop(t2)
	if s.Valid(t2) {
		t.Fatal("注销后应失效")
	}
	if s.Valid("garbage") {
		t.Fatal("伪造 token 应失效")
	}
}
