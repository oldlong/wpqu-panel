package admin

import (
	"crypto/ed25519"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"wpqu-panel/internal/signing"
)

// Version 是管理后台自身的版本，和面板版本无关 —— 它只影响这个工具。
const Version = "1.0.0"

const (
	cookieName = "wpqu_admin"
	maxBody    = 1 << 20 // JSON 请求体上限
)

//go:embed public
var publicFS embed.FS

// Server 是管理后台的 HTTP 服务。
type Server struct {
	cfg      Config
	auth     *Auth
	sessions *Sessions
	limiter  *limiter
	ledger   *Ledger
	audit    *Audit
	started  time.Time

	releaseKey ed25519.PrivateKey
	licenseKey ed25519.PrivateKey
	// licensePub 是本机授权私钥对应的公钥。
	//
	// 验码用它，而不是直接用编译进面板的常量：两者一致时结果相同，
	// 不一致时说明密钥配错了 —— 这时候必须告诉用户「面板会拒绝这个码」，
	// 否则验码页会给出一个让人放心的假结论。
	licensePub string

	// writeMu 把打包、写文件、发码串起来。
	// 这些操作都会改 site/ 或台账，并发跑没有收益，只会让「谁覆盖了谁」变难查。
	writeMu sync.Mutex
}

// New 加载私钥、口令与台账。缺任何一样都直接失败，不做半可用状态。
func New(cfg Config) (*Server, error) {
	release, err := signing.LoadPrivateKey(cfg.keyPath("release"))
	if err != nil {
		return nil, fmt.Errorf("%w\n提示：把 release.key 放到 %s，或用 -keys 指向正确目录", err, cfg.KeysDir)
	}
	license, err := signing.LoadPrivateKey(cfg.keyPath("license"))
	if err != nil {
		return nil, fmt.Errorf("%w\n提示：把 license.key 放到 %s，或用 -keys 指向正确目录", err, cfg.KeysDir)
	}
	if err := ensureDir(cfg.DataDir); err != nil {
		return nil, err
	}
	auth, err := LoadAuth(dataFile(cfg, "auth.json"))
	if err != nil {
		return nil, fmt.Errorf("读口令失败（正常启动会自动生成，这里多半是文件损坏）：%w", err)
	}
	ledger, err := LoadLedger(dataFile(cfg, "licenses.ndjson"))
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:        cfg,
		auth:       auth,
		sessions:   NewSessions(time.Duration(cfg.SessionHours) * time.Hour),
		limiter:    newLimiter(),
		ledger:     ledger,
		audit:      &Audit{path: dataFile(cfg, "audit.ndjson")},
		started:    time.Now(),
		releaseKey: release,
		licenseKey: license,
		licensePub: signing.PublicKeyB64(license),
	}, nil
}

// Keys 暴露两个私钥，供命令行场景复用（例如 -check）。
func (s *Server) Keys() (release, license ed25519.PrivateKey) {
	return s.releaseKey, s.licenseKey
}

// ---------------------------------------------------------------- 路由

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			log.Printf("[panic] %s %s: %v", r.Method, r.URL.Path, v)
			http.Error(w, `{"error":"服务内部错误"}`, http.StatusInternalServerError)
		}
	}()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")

	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.api(w, r)
		return
	}
	s.static(w, r)
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	p := path.Clean(r.URL.Path)
	w.Header().Set("Cache-Control", "no-store")

	// 登录相关不要求已有会话
	switch p {
	case "/api/login":
		s.handleLogin(w, r)
		return
	case "/api/logout":
		s.handleLogout(w, r)
		return
	case "/api/ping":
		// 给「双击第二次」用：新进程靠它认出已经有一个实例在跑，
		// 于是直接把浏览器指过去，而不是再起一份去抢同一批数据文件。
		writeJSON(w, http.StatusOK, map[string]any{
			"app": "wpquadmin", "version": Version, "startedAt": s.started,
		})
		return
	}

	if !s.authed(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录或登录已过期"})
		return
	}
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "跨站请求被拒绝"})
		return
	}
	// 所有写操作都要求这个头：它是跨站请求无法伪造的一道门槛。
	if r.Method != http.MethodGet && r.Header.Get("X-Admin") != "1" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "缺少 X-Admin 请求头"})
		return
	}

	parts := strings.Split(strings.Trim(p, "/"), "/") // ["api", ...]
	head := ""
	if len(parts) > 1 {
		head = parts[1]
	}
	sub := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}

	switch head {
	case "me":
		s.handleMe(w, r)
	case "password":
		s.handlePassword(w, r)
	case "overview":
		s.handleOverview(w, r)
	case "audit":
		s.handleAudit(w, r)
	case "plugins":
		s.routePlugins(w, r, parts)
	case "market":
		s.routeMarket(w, r, sub(2))
	case "site":
		s.routeSite(w, r, sub(2))
	case "updates":
		s.routeUpdates(w, r, sub(2))
	case "licenses":
		s.routeLicenses(w, r, parts)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "没有这个接口"})
	}
}

// static 提供内置界面。界面里没有任何外部资源，断网也能用。
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	sub, err := fs.Sub(publicFS, "public")
	if err != nil {
		http.Error(w, "内置资源缺失", http.StatusInternalServerError)
		return
	}
	f, err := sub.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || stat.IsDir() {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	// HTML 不缓存，避免改完前端看不到效果；静态资源用时间戳做版本号由 HTML 负责
	if strings.HasSuffix(name, "index.html") {
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = io.Copy(w, f)
}

// ---------------------------------------------------------------- 认证

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return s.sessions.Valid(c.Value)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	ip := clientIP(r, s.cfg.BehindProxy)
	if ok, wait := s.limiter.allow(ip); !ok {
		writeJSON(w, http.StatusTooManyRequests,
			map[string]any{"error": fmt.Sprintf("尝试过于频繁，请 %d 秒后再试", int(wait.Seconds()))})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !s.auth.Verify(req.Password) {
		s.limiter.fail(ip)
		s.audit.Add(ip, "login.fail", s.auth.User, "")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "口令不正确"})
		return
	}
	s.limiter.reset(ip)
	token, exp := s.sessions.New()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r, s.cfg.BehindProxy),
		Expires:  exp,
	})
	s.audit.Add(ip, "login.ok", s.auth.User, "")
	writeJSON(w, http.StatusOK, map[string]any{"user": s.auth.User, "expires": exp})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessions.Drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user":      s.auth.User,
		"version":   Version,
		"startedAt": s.started.Format(time.RFC3339),
	})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !s.auth.Verify(req.Old) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "原口令不正确"})
		return
	}
	a, err := NewAuth(s.auth.User, req.New)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := a.Save(dataFile(s.cfg, "auth.json")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "写口令失败：" + err.Error()})
		return
	}
	s.auth = a
	s.audit.Add(clientIP(r, s.cfg.BehindProxy), "password.change", s.auth.User, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 工具

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return errors.New("读请求失败")
	}
	if len(body) == 0 {
		return errors.New("请求体为空")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求格式不对：%w", err)
	}
	return nil
}

// sameOrigin 拒绝来自其它站点的请求。
//
// SameSite=Strict 已经挡掉大部分情况，但浏览器行为并非铁板一块，
// 这里再按 Origin 核一次，代价只有几行。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // 非浏览器客户端（curl / 脚本）
	}
	host := r.Host
	if i := strings.Index(origin, "://"); i > 0 {
		origin = origin[i+3:]
	}
	return strings.EqualFold(origin, host)
}

func clientIP(r *http.Request, behindProxy bool) string {
	if behindProxy {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			if i := strings.Index(v, ","); i > 0 {
				return strings.TrimSpace(v[:i])
			}
			return strings.TrimSpace(v)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func isHTTPS(r *http.Request, behindProxy bool) bool {
	if r.TLS != nil {
		return true
	}
	return behindProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func ensureDir(p string) error {
	if p == "" {
		return errors.New("配置里缺少目录")
	}
	return os.MkdirAll(p, 0o700)
}

func dataFile(cfg Config, name string) string {
	return path.Join(cfg.DataDir, name)
}

// JSONField 从 map 里取一个字符串字段，取不到就给空。
func JSONField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
