// Package httpapi：HTTP 路由与静态资源。
// 路由表、参数校验、ndjson 流式协议与早期实现一一对应。
package httpapi

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"wpqu-panel/internal/dbx"
	"wpqu-panel/internal/ops"
	"wpqu-panel/internal/plugins"
	"wpqu-panel/internal/sites"
	"wpqu-panel/internal/sshc"
	"wpqu-panel/internal/store"
	"wpqu-panel/internal/updater"
)

// Version 与早期实现保持同一版本号，方便对照。
const Version = "1.9.2"

// App 聚合全部依赖。
type App struct {
	Store   *store.Store
	Pool    *sshc.Pool
	Runner  *ops.Runner
	Public  fs.FS
	Version string
	DataDir string

	Updater *updater.Client
	Plugins *plugins.Manager

	srv      *http.Server
	stopCh   chan struct{}
	stopOnce sync.Once
}

var mimeTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "application/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
}

// ---------------------------------------------------------------- 工具

func badRequest(msg string) error { return &dbx.BadRequest{Msg: msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	// 不转义 < > &：与早期实现输出一致，日志里的命令片段原样呈现
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	body := bytes.TrimRight(buf.Bytes(), "\n")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type errStatus struct{ status int }

func (e *errStatus) Error() string { return "" }

func writeErr(w http.ResponseWriter, err error) {
	var br *dbx.BadRequest
	var need *store.VaultNeedPassword
	var bad *store.VaultBadPassword
	var out *store.VaultLockedOut
	msg := err.Error()
	if msg == "" {
		msg = "服务器内部错误"
	}
	switch {
	case errors.As(err, &bad):
		// 密码错但还没到锁定次数
		writeJSON(w, 400, map[string]any{
			"error": msg, "code": "vault_bad_password", "remaining": bad.Remaining,
		})
	case errors.As(err, &out):
		// 连续输错触发的锁定
		writeJSON(w, 423, map[string]any{
			"error": msg, "code": "vault_lockout",
			"lockSeconds": out.Seconds, "remaining": 0,
		})
	case errors.As(err, &need):
		// 设了密码但没解锁：前端据此弹解锁框
		writeJSON(w, 423, map[string]any{
			"error": msg, "code": "vault_locked",
			"serverId": need.ServerID, "lockedUntil": need.LockedUntil,
		})
	case errors.As(err, &br):
		writeJSON(w, 400, map[string]string{"error": msg})
	default:
		writeJSON(w, 500, map[string]string{"error": msg})
	}
}

// readJSON 读请求体（带大小限制）。
func readJSON(r *http.Request, limit int64) (map[string]any, error) {
	if limit <= 0 {
		limit = 2 * 1024 * 1024
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, badRequest("读取请求体失败")
	}
	if int64(len(body)) > limit {
		return nil, badRequest(fmt.Sprintf("请求体超过限制（%d MB）", limit/1024/1024))
	}
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, badRequest("请求体不是合法 JSON")
	}
	return m, nil
}

func readRaw(r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, badRequest("读取请求体失败")
	}
	if int64(len(body)) > limit {
		return nil, badRequest(fmt.Sprintf("请求体超过限制（%d MB）", limit/1024/1024))
	}
	return body, nil
}

// ---------------------------------------------------------------- 校验

var labelRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
var protoRe = regexp.MustCompile(`^[a-z]+://`)
var tailRe = regexp.MustCompile(`[/?#].*$`)
var portRe = regexp.MustCompile(`:\d+$`)
var idRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,64}$`)

// AssertDomain 域名校验（带容错清洗）。
func AssertDomain(v any, field string) (string, error) {
	if field == "" {
		field = "域名"
	}
	d := strings.ToLower(strings.TrimSpace(str(v)))
	d = protoRe.ReplaceAllString(d, "")
	d = tailRe.ReplaceAllString(d, "")
	d = portRe.ReplaceAllString(d, "")
	d = strings.TrimSuffix(d, ".")
	if d == "" {
		return "", badRequest(field + "不能为空")
	}
	if len(d) > 253 {
		return "", badRequest(field + "过长")
	}
	for _, l := range strings.Split(d, ".") {
		if !labelRe.MatchString(l) {
			return "", badRequest(fmt.Sprintf("%s格式不正确：%s。只能包含字母、数字和连字符，例如 example.com 或 pc5000（不支持下划线、空格、中文）", field, str(v)))
		}
	}
	return d, nil
}

func AssertID(v string, field string) (string, error) {
	if field == "" {
		field = "参数"
	}
	if !idRe.MatchString(v) {
		return "", badRequest(field + "格式不正确")
	}
	return v, nil
}

func defaultAdminEmail(domain string) string {
	if strings.Contains(domain, ".") {
		return "admin@" + domain
	}
	return "admin@example.com"
}

var siteTypes = []string{"wordpress", "custom", "static"}
var dbEngines = []string{"mariadb", "mysql8", "mysql84", "mysql57"}

func assertDbEngine(v any) (string, error) {
	e := strings.ToLower(strings.TrimSpace(str(v)))
	if e == "" {
		e = "mariadb"
	}
	for _, x := range dbEngines {
		if x == e {
			return e, nil
		}
	}
	return "", badRequest("数据库只能是 mariadb / mysql84 / mysql57")
}

// 初始化时的 Web 服务器：留空 = Nginx（默认组合）
func assertWebServer(v any) (string, error) {
	w := strings.ToLower(strings.TrimSpace(str(v)))
	switch w {
	case "", "nginx":
		return "nginx", nil
	case "apache", "apache2":
		return "apache", nil
	}
	return "", badRequest("Web 服务器只能是 nginx 或 apache")
}

// 初始化时的 PHP 版本：留空 = 发行版默认。只接受 7.x / 8.x 这种主次版本号，
// 具体小版本由软件源决定，不让前端传任意字符串下去拼包名。
var phpVersionRe = regexp.MustCompile(`^[78]\.[0-9]$`)

func assertPHPVersion(v any) (string, error) {
	p := strings.TrimSpace(str(v))
	if p == "" || p == "auto" {
		return "", nil
	}
	if !phpVersionRe.MatchString(p) {
		return "", badRequest("PHP 版本格式应该是 8.3 这样，当前为：" + p)
	}
	return p, nil
}

func assertSiteType(v any, required bool) (string, error) {
	raw := strings.TrimSpace(str(v))
	if raw == "" && required {
		return "", badRequest("缺少站点类型 type")
	}
	if raw == "" {
		raw = "wordpress"
	}
	for _, x := range siteTypes {
		if x == raw {
			return raw, nil
		}
	}
	return "", badRequest("站点类型只能是 wordpress / custom / static")
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func boolDefault(v any, def bool) bool {
	if v == nil {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func atoiIn(v any, def, min, max int) int {
	n := def
	switch t := v.(type) {
	case float64:
		n = int(t)
	case string:
		if t != "" {
			if p, err := strconv.Atoi(t); err == nil {
				n = p
			}
		}
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n
}

// ---------------------------------------------------------------- 流式协议

type streamWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (s *streamWriter) event(obj map[string]any) {
	// 与早期实现一致：不转义 < > &
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(obj)
	_, _ = s.w.Write(buf.Bytes())
	if s.f != nil {
		s.f.Flush()
	}
}

func startStream(w http.ResponseWriter) *streamWriter {
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	f, _ := w.(http.Flusher)
	if f != nil {
		f.Flush()
	}
	return &streamWriter{w: w, f: f}
}

// ---------------------------------------------------------------- 路由

type routeEntry struct {
	method   string
	segments []string // ":name" 为参数段
	handler  func(app *App, w http.ResponseWriter, r *http.Request, params map[string]string)
}

var routes []routeEntry

func route(method, pattern string, h func(app *App, w http.ResponseWriter, r *http.Request, params map[string]string)) {
	routes = append(routes, routeEntry{method: method, segments: strings.Split(strings.Trim(pattern, "/"), "/"), handler: h})
}

// streamCall 封装与早期实现的 streamHandler 相同的行为。
type streamCall struct {
	validate func(body map[string]any) error
	describe func(body map[string]any) string
	run      func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error)
	logType  string
	// noDiagnose：退出码非 0 时不去翻远端运维日志。执行命令这类操作失败是常态，
	// 硬塞一段「失败诊断」只会干扰用户看报错
	noDiagnose bool
}

func streamRoute(pattern string, call streamCall) {
	route("POST", pattern, func(app *App, w http.ResponseWriter, r *http.Request, params map[string]string) {
		server, err := app.getServerOrThrow(params["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		// 参数校验必须在开流之前
		if call.validate != nil {
			if err := call.validate(body); err != nil {
				writeErr(w, err)
				return
			}
		}
		sw := startStream(w)
		started := time.Now()
		sw.event(map[string]any{"type": "start", "message": call.describe(body)})
		push := func(text string) {
			sw.event(map[string]any{"type": "out", "text": ops.StripANSI(text)})
		}
		res, err := call.run(app, server, body, push)
		if err != nil {
			var de *ops.DoctorError
			if errors.As(err, &de) {
				ops.ReportDoctor(push, de.Doctor)
			}
			sw.event(map[string]any{"type": "error", "message": ops.StripANSI(err.Error())})
			_ = app.Store.AppendLog(map[string]any{
				"type": call.logType, "serverId": server.ID, "target": server.Host,
				"detail": call.describe(body), "result": "error", "error": err.Error(),
			})
			return
		}
		ok := res.Code == 0
		if !ok && !call.noDiagnose {
			app.Runner.ReportFailure(toSSH(server), push, ops.StripANSI(res.Stdout+res.Stderr), call.logType)
		}
		timeoutNote := ""
		if res.TimedOut {
			timeoutNote = "（超时）"
		}
		sw.event(map[string]any{
			"type": "end", "code": res.Code, "ok": ok,
			"durationMs": time.Since(started).Milliseconds(),
			"message":    ternary(ok, "执行完成", fmt.Sprintf("命令退出码 %d%s", res.Code, timeoutNote)),
		})
		_ = app.Store.AppendLog(map[string]any{
			"type": call.logType, "serverId": server.ID, "target": server.Host,
			"detail": call.describe(body), "result": ternary(ok, "ok", "fail"),
			"durationMs": time.Since(started).Milliseconds(),
		})
	})
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// toSSH 把存储记录转成 SSH 连接参数。
func toSSH(s *store.ServerWithSecret) sshc.Server {
	out := sshc.Server{ID: s.ID, Host: s.Host, Port: s.Port, Username: s.Username, AuthType: s.AuthType}
	if s.Credentials != nil {
		out.Password = s.Credentials.Password
		out.PrivateKey = s.Credentials.PrivateKey
		out.Passphrase = s.Credentials.Passphrase
	}
	return out
}

func (app *App) getServerOrThrow(id string) (*store.ServerWithSecret, error) {
	clean, err := AssertID(id, "")
	if err != nil {
		return nil, err
	}
	server, err := app.Store.GetServerWithSecret(clean)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, errors.New("服务器不存在")
	}
	if server.Locked {
		return nil, &store.VaultNeedPassword{ServerID: server.ID, Name: server.Name, LockedUntil: server.LockedUntil}
	}
	if server.Credentials == nil {
		return nil, errors.New("该服务器的凭据读取失败，请重新编辑并填写密码或私钥")
	}
	return server, nil
}

// ---------------------------------------------------------------- 路由注册

func init() {
	route("GET", "/api/ping", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		// impl 让启动器能分辨端口上跑的是本次构建还是早期版本
		writeJSON(w, 200, map[string]any{"ok": true, "impl": "go", "version": app.Version, "time": time.Now().UTC().Format("2006-01-02T15:04:05.000Z")})
	})

	route("POST", "/api/shutdown", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		// 无控制台模式（双击启动）靠它退出进程
		writeJSON(w, 200, map[string]any{"ok": true})
		go func() {
			time.Sleep(120 * time.Millisecond)
			app.RequestStop()
		}()
	})

	route("GET", "/api/servers", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		list, err := app.Store.ListServers()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"servers": list})
	})

	route("POST", "/api/servers", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		if str(body["host"]) == "" {
			writeErr(w, badRequest("请填写服务器地址"))
			return
		}
		authType := str(body["authType"])
		if authType == "key" {
			if str(body["privateKey"]) == "" {
				writeErr(w, badRequest("请粘贴私钥内容"))
				return
			}
		} else if str(body["password"]) == "" {
			writeErr(w, badRequest("请填写密码"))
			return
		}
		in := store.ServerInput{
			Name: str(body["name"]), Host: str(body["host"]),
			Port: intOf(body["port"]), Username: str(body["username"]),
			AuthType: authType, Note: str(body["note"]),
			Password: str(body["password"]), PrivateKey: str(body["privateKey"]),
			Passphrase: str(body["passphrase"]),
		}
		server, err := app.Store.AddServer(in)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{"type": "server.add", "serverId": server.ID, "target": server.Host, "result": "ok"})
		writeJSON(w, 200, map[string]any{"server": server})
	})

	route("PATCH", "/api/servers/:id", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		id, err := AssertID(p["id"], "")
		if err != nil {
			writeErr(w, err)
			return
		}
		in := store.ServerInput{
			Name: str(body["name"]), Host: str(body["host"]),
			Port: intOf(body["port"]), Username: str(body["username"]),
			AuthType: str(body["authType"]), Note: str(body["note"]),
			Password: str(body["password"]), PrivateKey: str(body["privateKey"]),
			Passphrase: str(body["passphrase"]),
		}
		updated, err := app.Store.UpdateServer(id, in)
		if err != nil {
			writeErr(w, err)
			return
		}
		if updated == nil {
			writeJSON(w, 404, map[string]string{"error": "服务器不存在"})
			return
		}
		app.Pool.CloseConnection(id)
		writeJSON(w, 200, map[string]any{"server": updated})
	})

	route("DELETE", "/api/servers/:id", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		id, err := AssertID(p["id"], "")
		if err != nil {
			writeErr(w, err)
			return
		}
		app.Pool.CloseConnection(id)
		ok, err := app.Store.RemoveServer(id)
		if err != nil {
			writeErr(w, err)
			return
		}
		result := "ok"
		if !ok {
			result = "missing"
		}
		_ = app.Store.AppendLog(map[string]any{"type": "server.remove", "serverId": id, "result": result})
		writeJSON(w, 200, map[string]any{"ok": ok})
	})

	// ------------------------------------------------------------ 本地加密
	// 给单台服务器设一个本机密码：凭据改用密码派生的密钥加密，不校验强度，
	// 连续 5 次输错锁 1 小时。详见 store/vault.go。

	route("GET", "/api/servers/:id/vault", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		id, err := AssertID(p["id"], "")
		if err != nil {
			writeErr(w, err)
			return
		}
		info, err := app.Store.VaultInfo(id)
		if err != nil {
			writeErr(w, err)
			return
		}
		if info == nil {
			writeJSON(w, 404, map[string]string{"error": "服务器不存在"})
			return
		}
		writeJSON(w, 200, map[string]any{"vault": info})
	})

	// POST 设置（首次）或修改（带 oldPassword）密码
	route("POST", "/api/servers/:id/vault", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		id, err := AssertID(p["id"], "")
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		password := str(body["password"])
		old := str(body["oldPassword"])
		var info *store.VaultInfo
		action := "server.vault.set"
		if old == "" {
			info, err = app.Store.SetVaultPassword(id, password)
		} else {
			action = "server.vault.change"
			info, err = app.Store.ChangeVaultPassword(id, old, password)
		}
		if err != nil {
			_ = app.Store.AppendLog(map[string]any{"type": action, "serverId": id, "result": "error", "error": err.Error()})
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{"type": action, "serverId": id, "result": "ok"})
		writeJSON(w, 200, map[string]any{"vault": info})
	})

	route("POST", "/api/servers/:id/vault/unlock", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		id, err := AssertID(p["id"], "")
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		info, err := app.Store.UnlockVault(id, str(body["password"]))
		if err != nil {
			var bad *store.VaultBadPassword
			var out *store.VaultLockedOut
			result := "fail"
			if errors.As(err, &bad) || errors.As(err, &out) {
				result = "denied"
			}
			_ = app.Store.AppendLog(map[string]any{"type": "server.vault.unlock", "serverId": id, "result": result, "error": err.Error()})
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{"type": "server.vault.unlock", "serverId": id, "result": "ok"})
		writeJSON(w, 200, map[string]any{"vault": info})
	})

	// 立即锁定：只清掉内存里的密钥，不动密码
	route("POST", "/api/servers/:id/vault/lock", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		id, err := AssertID(p["id"], "")
		if err != nil {
			writeErr(w, err)
			return
		}
		app.Store.LockVault(id)
		info, err := app.Store.VaultInfo(id)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{"type": "server.vault.lock", "serverId": id, "result": "ok"})
		writeJSON(w, 200, map[string]any{"vault": info})
	})

	// DELETE 关闭加密，需要当前密码
	route("DELETE", "/api/servers/:id/vault", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		id, err := AssertID(p["id"], "")
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		info, err := app.Store.RemoveVault(id, str(body["password"]))
		if err != nil {
			_ = app.Store.AppendLog(map[string]any{"type": "server.vault.remove", "serverId": id, "result": "error", "error": err.Error()})
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{"type": "server.vault.remove", "serverId": id, "result": "ok"})
		writeJSON(w, 200, map[string]any{"vault": info})
	})

	route("POST", "/api/servers/:id/test", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		info, err := app.Pool.TestConnection(toSSH(server))
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.MarkServer(server.ID, map[string]any{
			"lastConnectedAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			"lastStatus":      "ok",
			"os":              info["os"],
			"hostname":        info["hostname"],
		})
		_ = app.Store.AppendLog(map[string]any{"type": "server.test", "serverId": server.ID, "target": server.Host, "result": "ok"})
		writeJSON(w, 200, map[string]any{"ok": true, "info": info})
	})

	route("GET", "/api/servers/:id/doctor", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		result, err := app.Runner.Doctor(toSSH(server))
		if err != nil {
			writeErr(w, err)
			return
		}
		var bad []string
		for _, c := range result.Checks {
			if !c.OK {
				bad = append(bad, c.Label)
			}
		}
		detail := "全部通过"
		if len(bad) > 0 {
			detail = strings.Join(bad, ",")
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "server.doctor", "serverId": server.ID, "target": server.Host,
			"result": ternary(result.OK, "ok", "warn"), "detail": detail,
		})
		writeJSON(w, 200, result)
	})

	route("GET", "/api/servers/:id/overview", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		snapshot, err := sites.GetServerSnapshot(app.Pool, toSSH(server))
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.MarkServer(server.ID, map[string]any{
			"lastConnectedAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			"lastStatus":      "ok",
		})
		writeJSON(w, 200, snapshot)
	})

	route("GET", "/api/servers/:id/site-detail", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		domain, err := AssertDomain(r.URL.Query().Get("domain"), "域名")
		if err != nil {
			writeErr(w, err)
			return
		}
		detail, err := sites.GetSiteDetail(app.Pool, toSSH(server), domain)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, detail)
	})

	// --- 建站 / 转换 / 初始化 / 域名（流式）

	streamRoute("/api/servers/:id/sites", streamCall{
		logType: "site.add",
		describe: func(b map[string]any) string {
			t := str(b["type"])
			if t == "" {
				t = "wordpress"
			}
			return fmt.Sprintf("新增站点 %s（%s）", str(b["domain"]), t)
		},
		validate: func(b map[string]any) error {
			if _, err := AssertDomain(b["domain"], "域名"); err != nil {
				return err
			}
			_, err := assertSiteType(b["type"], false)
			return err
		},
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			domain, _ := AssertDomain(body["domain"], "域名")
			t, _ := assertSiteType(body["type"], false)
			title := str(body["title"])
			if title == "" {
				title = str(body["domain"])
			}
			adminUser := str(body["adminUser"])
			if adminUser == "" {
				adminUser = "admin"
			}
			locale := "zh_CN"
			if str(body["locale"]) == "en_US" {
				locale = "en_US"
			}
			separate := "0"
			if boolDefault(body["separatePool"], false) {
				separate = "1"
			}
			return app.Runner.AddSite(toSSH(server), ops.AddSiteParams{
				Domain: domain, Type: t, Title: title,
				AdminUser: adminUser, AdminPass: str(body["adminPass"]),
				AdminEmail: defaultAdminEmail(domain), Locale: locale,
				MaxChildren:  strconv.Itoa(atoiIn(body["maxChildren"], 4, 1, 12)),
				SeparatePool: separate, SSL: boolDefault(body["ssl"], true),
			}, push)
		},
	})

	streamRoute("/api/servers/:id/site-type", streamCall{
		logType: "site.type",
		describe: func(b map[string]any) string {
			return fmt.Sprintf("切换站点能力 %s → %s", str(b["domain"]), str(b["type"]))
		},
		validate: func(b map[string]any) error {
			if _, err := AssertDomain(b["domain"], "域名"); err != nil {
				return err
			}
			_, err := assertSiteType(b["type"], true)
			return err
		},
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			domain, _ := AssertDomain(body["domain"], "域名")
			t, _ := assertSiteType(body["type"], true)
			title := str(body["title"])
			if title == "" {
				title = str(body["domain"])
			}
			adminUser := str(body["adminUser"])
			if adminUser == "" {
				adminUser = "admin"
			}
			locale := "zh_CN"
			if str(body["locale"]) == "en_US" {
				locale = "en_US"
			}
			separate := "0"
			if boolDefault(body["separatePool"], false) {
				separate = "1"
			}
			return app.Runner.ConvertSiteType(toSSH(server), ops.ConvertParams{
				Domain: domain, TargetType: t, Title: title,
				AdminUser: adminUser, AdminPass: str(body["adminPass"]),
				AdminEmail: defaultAdminEmail(domain), Locale: locale,
				MaxChildren:  strconv.Itoa(atoiIn(body["maxChildren"], 4, 1, 12)),
				SeparatePool: separate,
				Force:        boolDefault(body["force"], false),
				DryRun:       boolDefault(body["dryRun"], false),
			}, push)
		},
	})

	streamRoute("/api/servers/:id/bootstrap", streamCall{
		logType: "server.bootstrap",
		describe: func(b map[string]any) string {
			engine := "MariaDB"
			if assertEngineIs(b, "mysql84") || assertEngineIs(b, "mysql8") {
				engine = "MySQL 8.4"
			} else if assertEngineIs(b, "mysql57") {
				engine = "MySQL 5.7"
			}
			web := "Nginx"
			if w, _ := assertWebServer(b["webServer"]); w == "apache" {
				web = "Apache"
			}
			php, _ := assertPHPVersion(b["phpVersion"])
			stack := web + " + " + engine
			if php != "" {
				stack = web + " + PHP " + php + " + " + engine
			}
			return fmt.Sprintf("初始化环境并部署 %s（%s）", str(b["domain"]), stack)
		},
		validate: func(b map[string]any) error {
			if _, err := AssertDomain(b["domain"], "域名"); err != nil {
				return err
			}
			if _, err := assertDbEngine(b["dbEngine"]); err != nil {
				return err
			}
			if _, err := assertWebServer(b["webServer"]); err != nil {
				return err
			}
			_, err := assertPHPVersion(b["phpVersion"])
			return err
		},
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			domain, _ := AssertDomain(body["domain"], "域名")
			engine, _ := assertDbEngine(body["dbEngine"])
			web, _ := assertWebServer(body["webServer"])
			php, _ := assertPHPVersion(body["phpVersion"])
			title := str(body["title"])
			if title == "" {
				title = str(body["domain"])
			}
			adminUser := str(body["adminUser"])
			if adminUser == "" {
				adminUser = "admin"
			}
			return app.Runner.BootstrapServer(toSSH(server), ops.BootstrapParams{
				Domain: domain, Title: title,
				AdminUser: adminUser, AdminPass: str(body["adminPass"]),
				AdminEmail: defaultAdminEmail(domain),
				WebServer:  web, PHPVersion: php, DBEngine: engine,
				SSL: boolDefault(body["ssl"], true), UFW: boolDefault(body["ufw"], true),
				Redis: boolDefault(body["redis"], false),
			}, push)
		},
	})

	streamRoute("/api/servers/:id/domain", streamCall{
		logType: "site.domain",
		describe: func(b map[string]any) string {
			return fmt.Sprintf("变更域名 %s → %s", str(b["oldDomain"]), str(b["newDomain"]))
		},
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			old, err := AssertDomain(body["oldDomain"], "原域名")
			if err != nil {
				return nil, err
			}
			nw, err := AssertDomain(body["newDomain"], "新域名")
			if err != nil {
				return nil, err
			}
			return app.Runner.ChangeDomain(toSSH(server), ops.DomainParams{
				OldDomain: old, NewDomain: nw,
				KeepRedirect: boolDefault(body["keepRedirect"], true),
				MoveDir:      boolDefault(body["moveDir"], true),
				SSL:          boolDefault(body["ssl"], true),
				DryRun:       boolDefault(body["dryRun"], false),
			}, push)
		},
	})

	streamRoute("/api/servers/:id/site-delete", streamCall{
		logType: "site.delete",
		describe: func(b map[string]any) string {
			return "删除整站 " + str(b["domain"])
		},
		validate: func(b map[string]any) error {
			_, err := AssertDomain(b["domain"], "域名")
			return err
		},
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			domain, err := AssertDomain(body["domain"], "域名")
			if err != nil {
				return nil, err
			}
			return app.Runner.DeleteSite(toSSH(server), ops.DeleteSiteParams{
				Domain:      domain,
				DBName:      str(body["dbName"]),
				DeleteFiles: boolDefault(body["deleteFiles"], true),
				DeleteDB:    boolDefault(body["deleteDb"], true),
				DeleteCert:  boolDefault(body["deleteCert"], true),
			}, push)
		},
	})

	// ---------------------------------------------------------------- FTP（可选组件）

	route("GET", "/api/servers/:id/ftp", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		st, _, err := app.Runner.FTP(toSSH(server), ops.FTPParams{Action: "status"}, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "status": st})
	})

	streamRoute("/api/servers/:id/ftp/install", streamCall{
		logType:  "ftp.install",
		describe: func(b map[string]any) string { return "安装 FTP 组件" },
		validate: func(b map[string]any) error { return validateFTPConfig(b) },
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			p := ftpParams(body)
			p.Action = "install"
			if p.PasvAddr == "" {
				p.PasvAddr = "" // 交给脚本自动探测（配置里留空也行）
			}
			_, res, err := app.Runner.FTP(toSSH(server), p, push)
			return res, err
		},
	})

	streamRoute("/api/servers/:id/ftp/uninstall", streamCall{
		logType:  "ftp.uninstall",
		describe: func(b map[string]any) string { return "卸载 FTP 组件" },
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			_, res, err := app.Runner.FTP(toSSH(server), ops.FTPParams{Action: "uninstall"}, push)
			return res, err
		},
	})

	// control / config / users 都是秒级操作，直接返回最新状态
	route("POST", "/api/servers/:id/ftp/control", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		action := strings.TrimSpace(str(body["action"]))
		switch action {
		case "start", "stop", "restart":
		default:
			writeErr(w, badRequest("只支持 start / stop / restart"))
			return
		}
		st, _, err := app.Runner.FTP(toSSH(server), ops.FTPParams{Action: action}, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{"type": "ftp.service", "serverId": server.ID, "detail": action, "result": "ok"})
		writeJSON(w, 200, map[string]any{"ok": true, "status": st})
	})

	route("POST", "/api/servers/:id/ftp/config", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		if err := validateFTPConfig(body); err != nil {
			writeErr(w, err)
			return
		}
		fp := ftpParams(body)
		fp.Action = "config"
		st, _, err := app.Runner.FTP(toSSH(server), fp, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "ftp.config", "serverId": server.ID,
			"detail": fmt.Sprintf("被动端口 %d-%d", fp.PasvMin, fp.PasvMax), "result": "ok",
		})
		writeJSON(w, 200, map[string]any{"ok": true, "status": st})
	})

	route("POST", "/api/servers/:id/ftp/users", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		name := strings.TrimSpace(str(body["name"]))
		if !ftpNameRe.MatchString(name) {
			writeErr(w, badRequest("用户名只能用 3-32 位小写字母、数字、下划线、连字符，且以字母开头"))
			return
		}
		password := str(body["password"])
		if err := assertFTPPassword(password); err != nil {
			writeErr(w, err)
			return
		}
		dir, err := assertFTPDir(str(body["dir"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		st, res, err := app.Runner.FTP(toSSH(server), ops.FTPParams{
			Action: "adduser", User: name, Password: password, Dir: dir,
			ReadOnly: !boolDefault(body["writable"], true),
		}, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "ftp.adduser", "serverId": server.ID, "target": name,
			"detail": dir, "result": "ok",
		})
		out := ""
		if res != nil {
			out = ftpLogText(res.Stdout)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "status": st, "log": out})
	})

	route("POST", "/api/servers/:id/ftp/users/delete", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		name := strings.TrimSpace(str(body["name"]))
		if !ftpNameRe.MatchString(name) {
			writeErr(w, badRequest("用户名不合法"))
			return
		}
		st, _, err := app.Runner.FTP(toSSH(server), ops.FTPParams{Action: "deluser", User: name}, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{"type": "ftp.deluser", "serverId": server.ID, "target": name, "result": "ok"})
		writeJSON(w, 200, map[string]any{"ok": true, "status": st})
	})

	route("POST", "/api/servers/:id/ftp/users/dir", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		name := strings.TrimSpace(str(body["name"]))
		if !ftpNameRe.MatchString(name) {
			writeErr(w, badRequest("用户名不合法"))
			return
		}
		dir, err := assertFTPDir(str(body["dir"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		st, res, err := app.Runner.FTP(toSSH(server), ops.FTPParams{Action: "setdir", User: name, Dir: dir}, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "ftp.setdir", "serverId": server.ID, "target": name, "detail": dir, "result": "ok",
		})
		writeJSON(w, 200, map[string]any{"ok": true, "status": st, "log": ftpLogText(res.Stdout)})
	})

	route("POST", "/api/servers/:id/ftp/users/password", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		name := strings.TrimSpace(str(body["name"]))
		if !ftpNameRe.MatchString(name) {
			writeErr(w, badRequest("用户名不合法"))
			return
		}
		password := str(body["password"])
		if err := assertFTPPassword(password); err != nil {
			writeErr(w, err)
			return
		}
		_, res, err := app.Runner.FTP(toSSH(server), ops.FTPParams{Action: "setpass", User: name, Password: password}, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{"type": "ftp.setpass", "serverId": server.ID, "target": name, "result": "ok"})
		writeJSON(w, 200, map[string]any{"ok": true, "log": ftpLogText(res.Stdout)})
	})

	route("POST", "/api/servers/:id/ftp/users/mode", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		name := strings.TrimSpace(str(body["name"]))
		if !ftpNameRe.MatchString(name) {
			writeErr(w, badRequest("用户名不合法"))
			return
		}
		writable := boolDefault(body["writable"], true)
		st, _, err := app.Runner.FTP(toSSH(server), ops.FTPParams{
			Action: "setrw", User: name, ReadOnly: !writable,
		}, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "ftp.setrw", "serverId": server.ID, "target": name,
			"detail": map[bool]string{true: "读写", false: "只读"}[writable], "result": "ok",
		})
		writeJSON(w, 200, map[string]any{"ok": true, "status": st})
	})

	// ---------------------------------------------------------------- 维护：PHP 版本

	route("GET", "/api/servers/:id/php", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		st, err := app.Runner.PHPStatus(toSSH(server))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "status": st})
	})

	streamRoute("/api/servers/:id/php/install", streamCall{
		logType:  "php.install",
		describe: func(b map[string]any) string { return "安装 PHP " + strings.TrimSpace(str(b["version"])) },
		validate: func(b map[string]any) error { _, err := assertPHPVer(b["version"]); return err },
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			v, _ := assertPHPVer(body["version"])
			f := &panelLineFilter{push: push}
			_, res, err := app.Runner.PHPAction(toSSH(server), "install", v, "", false, f.Write)
			f.Flush()
			return res, err
		},
	})

	streamRoute("/api/servers/:id/php/repo", streamCall{
		logType:  "php.repo",
		describe: func(b map[string]any) string { return "添加 PHP 软件源" },
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			f := &panelLineFilter{push: push}
			_, res, err := app.Runner.PHPAction(toSSH(server), "repo", "", "", false, f.Write)
			f.Flush()
			return res, err
		},
	})

	streamRoute("/api/servers/:id/php/remove", streamCall{
		logType:  "php.remove",
		describe: func(b map[string]any) string { return "卸载 PHP " + strings.TrimSpace(str(b["version"])) },
		validate: func(b map[string]any) error { _, err := assertPHPVer(b["version"]); return err },
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			v, _ := assertPHPVer(body["version"])
			f := &panelLineFilter{push: push}
			_, res, err := app.Runner.PHPAction(toSSH(server), "remove", v, "", boolDefault(body["force"], false), f.Write)
			f.Flush()
			return res, err
		},
	})

	route("POST", "/api/servers/:id/php/site", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		domain, err := AssertDomain(body["domain"], "站点域名")
		if err != nil {
			writeErr(w, err)
			return
		}
		ver, err := assertPHPVer(body["version"])
		if err != nil {
			writeErr(w, err)
			return
		}
		st, res, err := app.Runner.PHPAction(toSSH(server), "site", ver, domain, boolDefault(body["force"], false), nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "php.site", "serverId": server.ID, "target": domain,
			"detail": "PHP " + ver, "result": "ok",
		})
		log := ""
		if res != nil {
			log = cleanScriptLog(res.Stdout)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "status": st, "log": log})
	})

	route("POST", "/api/servers/:id/php/default", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		ver, err := assertPHPVer(body["version"])
		if err != nil {
			writeErr(w, err)
			return
		}
		st, res, err := app.Runner.PHPAction(toSSH(server), "default", ver, "", false, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "php.default", "serverId": server.ID, "detail": "CLI → PHP " + ver, "result": "ok",
		})
		log := ""
		if res != nil {
			log = cleanScriptLog(res.Stdout)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "status": st, "log": log})
	})

	// ---------------------------------------------------------------- 维护：网站日志

	route("GET", "/api/servers/:id/logs/list", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		files, err := app.Runner.Logs(toSSH(server))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "files": files})
	})

	route("POST", "/api/servers/:id/logs/tail", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		path := strings.TrimSpace(str(body["path"]))
		if err := assertLogPath(path); err != nil {
			writeErr(w, err)
			return
		}
		text, err := app.Runner.LogTail(toSSH(server), path, intOf(body["lines"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "text": text})
	})

	logStream := func(action, logType, label string) {
		streamRoute("/api/servers/:id/logs/"+action, streamCall{
			logType:  logType,
			describe: func(b map[string]any) string { return label + " " + strings.TrimSpace(str(b["path"])) },
			validate: func(b map[string]any) error {
				return assertLogPath(strings.TrimSpace(str(b["path"])))
			},
			run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
				path := strings.TrimSpace(str(body["path"]))
				f := &panelLineFilter{push: push}
				res, err := app.Runner.LogAction(toSSH(server), action, path, f.Write)
				f.Flush()
				return res, err
			},
		})
	}
	logStream("clear", "log.clear", "清空日志")
	logStream("rotate", "log.rotate", "轮转日志")
	logStream("delete", "log.delete", "删除归档")

	// ---------------------------------------------------------------- 维护：定时任务

	route("GET", "/api/servers/:id/cron", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		st, err := app.Runner.Cron(toSSH(server))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "status": st})
	})

	streamRoute("/api/servers/:id/cron/add", streamCall{
		logType: "cron.add",
		describe: func(b map[string]any) string {
			return "新增定时任务：" + strings.TrimSpace(str(b["schedule"]))
		},
		validate: func(b map[string]any) error {
			if _, err := validateCronSchedule(b["schedule"]); err != nil {
				return err
			}
			if strings.ContainsAny(str(b["label"]), "\r\n") {
				return badRequest("备注里不能有换行")
			}
			_, err := validateCronCommand(b["command"])
			return err
		},
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			sched, err := validateCronSchedule(body["schedule"])
			if err != nil {
				return nil, err
			}
			cmd, err := validateCronCommand(body["command"])
			if err != nil {
				return nil, err
			}
			f := &panelLineFilter{push: push}
			_, res, err := app.Runner.CronAction(toSSH(server), ops.CronParams{
				Action: "add", Schedule: sched, Command: cmd,
				Label: strings.TrimSpace(str(body["label"])),
			}, f.Write)
			f.Flush()
			return res, err
		},
	})

	streamRoute("/api/servers/:id/cron/delete", streamCall{
		logType:  "cron.delete",
		describe: func(b map[string]any) string { return "删除定时任务" },
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			f := &panelLineFilter{push: push}
			_, res, err := app.Runner.CronAction(toSSH(server), ops.CronParams{
				Action: "delete",
				ID:     strings.TrimSpace(str(body["id"])),
				Line:   intOf(body["line"]),
			}, f.Write)
			f.Flush()
			return res, err
		},
	})

	streamRoute("/api/servers/:id/cron/toggle", streamCall{
		logType: "cron.toggle",
		describe: func(b map[string]any) string {
			if boolDefault(b["enabled"], true) {
				return "启用定时任务"
			}
			return "停用定时任务"
		},
		validate: func(b map[string]any) error {
			if intOf(b["line"]) <= 0 {
				return badRequest("缺少任务行号，请刷新后重试")
			}
			return nil
		},
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			f := &panelLineFilter{push: push}
			_, res, err := app.Runner.CronAction(toSSH(server), ops.CronParams{
				Action: "toggle", Line: intOf(body["line"]),
				Enabled: boolDefault(body["enabled"], true),
			}, f.Write)
			f.Flush()
			return res, err
		},
	})

	streamRoute("/api/servers/:id/cron/run", streamCall{
		logType:    "cron.run",
		noDiagnose: true,
		describe:   func(b map[string]any) string { return "立即执行定时任务" },
		validate: func(b map[string]any) error {
			_, err := validateCronCommand(b["command"])
			return err
		},
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			cmd, err := validateCronCommand(body["command"])
			if err != nil {
				return nil, err
			}
			return app.Runner.RunCommand(toSSH(server), cmd, "", 3*time.Minute, push)
		},
	})

	// ---------------------------------------------------------------- 维护：执行命令

	streamRoute("/api/servers/:id/exec", streamCall{
		logType:    "server.exec",
		noDiagnose: true,
		describe: func(b map[string]any) string {
			cmd := strings.TrimSpace(str(b["command"]))
			r := []rune(cmd)
			if len(r) > 60 {
				cmd = string(r[:60]) + "…"
			}
			return "执行：" + cmd
		},
		validate: func(b map[string]any) error {
			cmd := strings.TrimSpace(str(b["command"]))
			if cmd == "" {
				return badRequest("命令不能为空")
			}
			if len(cmd) > 4000 {
				return badRequest("命令太长（最多 4000 字）")
			}
			return nil
		},
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			cmd := str(body["command"])
			cwd := strings.TrimSpace(str(body["cwd"]))
			if cwd != "" && !strings.HasPrefix(cwd, "/") {
				return nil, badRequest("工作目录必须是绝对路径")
			}
			secs := intOf(body["seconds"])
			if secs < 5 {
				secs = 120
			}
			if secs > 600 {
				secs = 600
			}
			return app.Runner.RunCommand(toSSH(server), cmd, cwd, time.Duration(secs)*time.Second, push)
		},
	})

	route("POST", "/api/servers/:id/backup", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		domain, err := AssertDomain(body["domain"], "域名")
		if err != nil {
			writeErr(w, err)
			return
		}
		sw := startStream(w)
		sw.event(map[string]any{"type": "start", "message": "备份 " + domain})
		files, err := app.Runner.BackupSite(toSSH(server), domain,
			boolDefault(body["includeDb"], true), boolDefault(body["includeFiles"], true),
			func(text string) { sw.event(map[string]any{"type": "out", "text": text}) })
		if err != nil {
			sw.event(map[string]any{"type": "error", "message": err.Error()})
			_ = app.Store.AppendLog(map[string]any{
				"type": "site.backup", "serverId": server.ID, "target": domain,
				"result": "error", "error": err.Error(),
			})
			return
		}
		sw.event(map[string]any{"type": "end", "ok": true, "files": files, "message": fmt.Sprintf("已保存 %d 个文件到本地", len(files))})
		var total int64
		for _, f := range files {
			total += f.Size
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "site.backup", "serverId": server.ID, "target": domain, "result": "ok",
			"detail": fmt.Sprintf("%d 个文件，%s", len(files), ops.FormatSize(total)),
		})
	})

	route("GET", "/api/servers/:id/backups", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		id, err := AssertID(p["id"], "")
		if err != nil {
			writeErr(w, err)
			return
		}
		files, err := app.Runner.ListBackups(id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"files": files})
	})

	streamRoute("/api/servers/:id/ssl/issue", streamCall{
		logType:  "ssl.issue",
		describe: func(b map[string]any) string { return "申请证书 " + str(b["domain"]) },
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			domain, err := AssertDomain(body["domain"], "域名")
			if err != nil {
				return nil, err
			}
			return app.Runner.IssueCert(toSSH(server), domain, str(body["email"]), push)
		},
	})

	streamRoute("/api/servers/:id/ssl/renew", streamCall{
		logType:  "ssl.renew",
		describe: func(b map[string]any) string { return "续期全部证书" },
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			return app.Runner.RenewCerts(toSSH(server), push)
		},
	})

	// --- 文件管理

	route("GET", "/api/files", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		q := r.URL.Query()
		server, err := app.getServerOrThrow(q.Get("server"))
		if err != nil {
			writeErr(w, err)
			return
		}
		dir := q.Get("path")
		if dir == "" {
			dir = "/"
		}
		if !strings.HasPrefix(dir, "/") {
			writeErr(w, badRequest("路径必须是绝对路径"))
			return
		}
		items, err := app.Pool.ListDir(toSSH(server), dir)
		if err != nil {
			writeErr(w, err)
			return
		}
		parent := (*string)(nil)
		if dir != "/" {
			d := filepath.ToSlash(filepath.Clean(dir))
			if i := strings.LastIndex(strings.TrimSuffix(d, "/"), "/"); i >= 0 {
				pd := strings.TrimSuffix(d, "/")[:i]
				if pd == "" {
					pd = "/"
				}
				parent = &pd
			}
		}
		writeJSON(w, 200, map[string]any{"path": dir, "parent": parent, "items": items})
	})

	route("GET", "/api/file", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		q := r.URL.Query()
		server, err := app.getServerOrThrow(q.Get("server"))
		if err != nil {
			writeErr(w, err)
			return
		}
		target := q.Get("path")
		if target == "" || !strings.HasPrefix(target, "/") {
			writeErr(w, badRequest("路径必须是绝对路径"))
			return
		}
		data, err := app.Pool.ReadFile(toSSH(server), target)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, data)
	})

	route("PUT", "/api/file", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		body, err := readJSON(r, 8*1024*1024)
		if err != nil {
			writeErr(w, err)
			return
		}
		server, err := app.getServerOrThrow(str(body["server"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		target := str(body["path"])
		if !strings.HasPrefix(target, "/") {
			writeErr(w, badRequest("路径必须是绝对路径"))
			return
		}
		content, ok := body["content"].(string)
		if !ok {
			writeErr(w, badRequest("缺少文件内容"))
			return
		}
		if err := app.Pool.WriteFile(toSSH(server), target, content); err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "file.write", "serverId": server.ID, "target": target,
			"result": "ok", "detail": fmt.Sprintf("%d 字节", len(content)),
		})
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	route("POST", "/api/file/upload", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		q := r.URL.Query()
		server, err := app.getServerOrThrow(q.Get("server"))
		if err != nil {
			writeErr(w, err)
			return
		}
		dir := q.Get("dir")
		name := q.Get("name")
		if dir == "" || !strings.HasPrefix(dir, "/") {
			writeErr(w, badRequest("目录必须是绝对路径"))
			return
		}
		if name == "" || strings.Contains(name, "/") {
			writeErr(w, badRequest("文件名不合法"))
			return
		}
		buf, err := readRaw(r, 200*1024*1024)
		if err != nil {
			writeErr(w, err)
			return
		}
		target := strings.TrimSuffix(dir, "/") + "/" + name
		if err := app.Pool.UploadBuffer(toSSH(server), target, buf); err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "file.upload", "serverId": server.ID, "target": target,
			"result": "ok", "detail": ops.FormatSize(int64(len(buf))),
		})
		writeJSON(w, 200, map[string]any{"ok": true, "size": len(buf)})
	})

	route("GET", "/api/file/download", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		q := r.URL.Query()
		server, err := app.getServerOrThrow(q.Get("server"))
		if err != nil {
			writeErr(w, err)
			return
		}
		target := q.Get("path")
		if target == "" || !strings.HasPrefix(target, "/") {
			writeErr(w, badRequest("路径必须是绝对路径"))
			return
		}
		st, err := app.Pool.StatPath(toSSH(server), target)
		if err != nil {
			writeErr(w, err)
			return
		}
		if st.Type == "dir" {
			writeErr(w, badRequest("不能直接下载目录"))
			return
		}
		base := filepath.Base(target)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size, 10))
		w.Header().Set("Content-Disposition", `attachment; filename="`+url.PathEscape(base)+`"`)
		w.WriteHeader(200)
		_ = app.Pool.StreamDownload(toSSH(server), target, w)
	})

	route("POST", "/api/file/mkdir", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		server, err := app.getServerOrThrow(str(body["server"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		dir := str(body["path"])
		if !strings.HasPrefix(dir, "/") {
			writeErr(w, badRequest("路径必须是绝对路径"))
			return
		}
		if err := app.Pool.Mkdir(toSSH(server), dir); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	route("POST", "/api/file/rename", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		server, err := app.getServerOrThrow(str(body["server"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		from, to := str(body["from"]), str(body["to"])
		if !strings.HasPrefix(from, "/") || !strings.HasPrefix(to, "/") {
			writeErr(w, badRequest("路径必须是绝对路径"))
			return
		}
		if err := app.Pool.Rename(toSSH(server), from, to); err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "file.rename", "serverId": server.ID, "target": from, "detail": to, "result": "ok",
		})
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	route("POST", "/api/file/delete", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		server, err := app.getServerOrThrow(str(body["server"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		target := str(body["path"])
		if !strings.HasPrefix(target, "/") {
			writeErr(w, badRequest("路径必须是绝对路径"))
			return
		}
		clean := strings.TrimRight(target, "/")
		if protectedPaths[clean] || len(strings.Split(clean, "/")) < 2 {
			writeErr(w, badRequest("该路径受保护，不允许删除"))
			return
		}
		cmd := "rm -rf -- " + sshc.ShellQuote(target)
		res, err := app.Pool.Exec(toSSH(server), sshc.WithRoot(toSSH(server), cmd), &sshc.ExecOptions{Timeout: 60 * time.Second})
		if err != nil {
			writeErr(w, err)
			return
		}
		if res.Code != 0 {
			msg := strings.TrimSpace(res.Stderr)
			if msg == "" {
				msg = "删除失败"
			}
			writeErr(w, errors.New(msg))
			return
		}
		_ = app.Store.AppendLog(map[string]any{"type": "file.delete", "serverId": server.ID, "target": target, "result": "ok"})
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	route("POST", "/api/file/chmod", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		server, err := app.getServerOrThrow(str(body["server"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		target := str(body["path"])
		if !strings.HasPrefix(target, "/") {
			writeErr(w, badRequest("路径必须是绝对路径"))
			return
		}
		mode := strings.TrimSpace(str(body["mode"]))
		if !octalModeRe.MatchString(mode) {
			writeErr(w, badRequest("权限必须是 3-4 位八进制数，例如 644 / 755 / 4755"))
			return
		}
		recursive, _ := body["recursive"].(bool)
		if err := app.Pool.Chmod(toSSH(server), target, mode, recursive); err != nil {
			writeErr(w, err)
			return
		}
		detail := mode
		if recursive {
			detail += "（含子项）"
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "file.chmod", "serverId": server.ID,
			"target": target, "detail": detail, "result": "ok",
		})
		writeJSON(w, 200, map[string]any{"ok": true, "mode": mode})
	})

	// 批量文件操作：选中多行后改权限 / 删除 / 复制 / 移动
	route("POST", "/api/file/batch", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		server, err := app.getServerOrThrow(str(body["server"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		action := strings.TrimSpace(str(body["action"]))
		raw, _ := body["paths"].([]any)
		paths := make([]string, 0, len(raw))
		seen := map[string]bool{}
		for _, v := range raw {
			origin := str(v)
			item := strings.TrimRight(origin, "/")
			if !strings.HasPrefix(item, "/") {
				if strings.HasPrefix(origin, "/") {
					writeErr(w, badRequest("不能对根目录执行该操作"))
				} else {
					writeErr(w, badRequest("路径必须是绝对路径"))
				}
				return
			}
			if seen[item] {
				continue
			}
			seen[item] = true
			paths = append(paths, item)
		}
		if len(paths) == 0 {
			writeErr(w, badRequest("请先选择文件或目录"))
			return
		}
		if len(paths) > 200 {
			writeErr(w, badRequest("一次最多 200 项，请分批操作"))
			return
		}

		sshSrv := toSSH(server)
		switch action {
		case "chmod":
			mode := strings.TrimSpace(str(body["mode"]))
			if !octalModeRe.MatchString(mode) {
				writeErr(w, badRequest("权限必须是 3-4 位八进制数，例如 644 / 755 / 4755"))
				return
			}
			recursive, _ := body["recursive"].(bool)
			for _, target := range paths {
				if err := app.Pool.Chmod(sshSrv, target, mode, recursive); err != nil {
					writeErr(w, fmt.Errorf("%s：%v", target, err))
					return
				}
			}
			_ = app.Store.AppendLog(map[string]any{
				"type": "file.chmod", "serverId": server.ID, "target": fmt.Sprintf("%d 项", len(paths)),
				"detail": mode, "result": "ok",
			})
			writeJSON(w, 200, map[string]any{"ok": true, "count": len(paths), "mode": mode})

		case "delete":
			for _, target := range paths {
				if protectedPaths[target] {
					writeErr(w, badRequest("该路径受保护，不允许删除："+target))
					return
				}
			}
			if err := app.Pool.RemoveAll(sshSrv, paths); err != nil {
				writeErr(w, err)
				return
			}
			_ = app.Store.AppendLog(map[string]any{
				"type": "file.delete", "serverId": server.ID, "target": fmt.Sprintf("%d 项", len(paths)),
				"detail": strings.Join(paths, " "), "result": "ok",
			})
			writeJSON(w, 200, map[string]any{"ok": true, "count": len(paths)})

		case "copy", "move":
			dest := strings.TrimSpace(str(body["dest"]))
			if !strings.HasPrefix(dest, "/") {
				writeErr(w, badRequest("目标目录必须是绝对路径"))
				return
			}
			for _, target := range paths {
				if target == dest {
					writeErr(w, badRequest("不能把目录移动/复制到它自己里面"))
					return
				}
			}
			var out *sshc.BatchResult
			if action == "copy" {
				out, err = app.Pool.Copy(sshSrv, paths, dest)
			} else {
				out, err = app.Pool.Move(sshSrv, paths, dest)
			}
			if err != nil {
				writeErr(w, err)
				return
			}
			label := "file.copy"
			if action == "move" {
				label = "file.move"
			}
			_ = app.Store.AppendLog(map[string]any{
				"type": label, "serverId": server.ID, "target": dest,
				"detail": fmt.Sprintf("%d 项", len(out.To)), "result": "ok",
			})
			writeJSON(w, 200, map[string]any{"ok": true, "count": len(out.To), "from": out.From, "to": out.To})

		default:
			writeErr(w, badRequest("不支持的操作："+action))
		}
	})

	route("GET", "/api/logs", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		limit := atoiIn(r.URL.Query().Get("limit"), 100, 1, 300)
		logs, err := app.Store.ReadLogs(limit)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"logs": logs})
	})

	// --- 数据库管理

	route("GET", "/api/servers/:id/db/databases", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		dbs, err := dbx.ListDatabases(app.Pool, toSSH(server))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"databases": dbs})
	})

	route("GET", "/api/servers/:id/db/tables", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		name, err := dbx.AssertDbName(r.URL.Query().Get("db"))
		if err != nil {
			writeErr(w, err)
			return
		}
		tables, err := dbx.ListTables(app.Pool, toSSH(server), name)
		if err != nil {
			writeErr(w, err)
			return
		}
		site, _ := dbx.FindSiteForDb(app.Pool, toSSH(server), name)
		writeJSON(w, 200, map[string]any{"db": name, "tables": tables, "site": site})
	})

	route("GET", "/api/servers/:id/db/rows", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		q := r.URL.Query()
		name, err := dbx.AssertDbName(q.Get("db"))
		if err != nil {
			writeErr(w, err)
			return
		}
		table, err := dbx.AssertTableName(q.Get("table"))
		if err != nil {
			writeErr(w, err)
			return
		}
		limit := atoiIn(q.Get("limit"), 50, 1, 200)
		offset := atoiIn(q.Get("offset"), 0, 0, 1<<30)
		result, err := dbx.FetchRows(app.Pool, toSSH(server), name, table, limit, offset)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, result)
	})

	route("GET", "/api/servers/:id/db/structure", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		q := r.URL.Query()
		name, err := dbx.AssertDbName(q.Get("db"))
		if err != nil {
			writeErr(w, err)
			return
		}
		table, err := dbx.AssertTableName(q.Get("table"))
		if err != nil {
			writeErr(w, err)
			return
		}
		kind := "columns"
		if q.Get("kind") == "indexes" {
			kind = "indexes"
		}
		result, err := dbx.TableInfo(app.Pool, toSSH(server), name, table, kind)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, result)
	})

	route("POST", "/api/servers/:id/db/query", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		name, err := dbx.AssertDbName(str(body["db"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		result, err := dbx.RunQuery(app.Pool, toSSH(server), name, str(body["sql"]), boolDefault(body["confirm"], false))
		if err != nil {
			writeErr(w, err)
			return
		}
		if result["kind"] == "write" {
			sql := strings.Join(strings.Fields(str(body["sql"])), " ")
			if len(sql) > 160 {
				sql = sql[:160]
			}
			_ = app.Store.AppendLog(map[string]any{
				"type": "db.query", "serverId": server.ID, "target": name,
				"detail": sql, "result": "ok", "durationMs": result["durationMs"],
			})
		}
		writeJSON(w, 200, result)
	})

	route("GET", "/api/servers/:id/db/export", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		name, err := dbx.AssertDbName(r.URL.Query().Get("db"))
		if err != nil {
			writeErr(w, err)
			return
		}
		dump, err := dbx.PrepareDump(app.Pool, toSSH(server), name)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{"type": "db.export", "serverId": server.ID, "target": name, "result": "ok"})
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.FormatInt(dump.Size, 10))
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="db-%s-%s.sql.gz"`, name, time.Now().UTC().Format("20060102150405")))
		w.WriteHeader(200)
		if err := app.Pool.StreamDownload(toSSH(server), dump.Remote, w); err != nil {
			log.Println("导出下载中断:", err)
		}
		dump.Finish()
	})

	route("POST", "/api/servers/:id/db/import", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		q := r.URL.Query()
		name, err := dbx.AssertDbName(q.Get("db"))
		if err != nil {
			writeErr(w, err)
			return
		}
		filename := q.Get("name")
		if filename == "" {
			filename = "upload.sql"
		}
		backupFirst := q.Get("backup") == "1"
		buf, err := readRaw(r, 300*1024*1024)
		if err != nil {
			writeErr(w, err)
			return
		}
		result, err := dbx.ImportDatabase(app.Pool, toSSH(server), name, filename, buf, backupFirst, app.Store.Paths.BackupDir)
		if err != nil {
			writeErr(w, err)
			return
		}
		detail := fmt.Sprintf("%s · %s", filename, ops.FormatSize(int64(len(buf))))
		if b, ok := result["backup"].(map[string]any); ok && b != nil {
			detail += fmt.Sprintf(" · 已备份 %v", b["name"])
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "db.import", "serverId": server.ID, "target": name,
			"detail": detail, "result": "ok",
		})
		writeJSON(w, 200, result)
	})

	route("POST", "/api/servers/:id/db/optimize", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		name, err := dbx.AssertDbName(str(body["db"]))
		if err != nil {
			writeErr(w, err)
			return
		}
		table := ""
		if t := str(body["table"]); t != "" {
			table, err = dbx.AssertTableName(t)
			if err != nil {
				writeErr(w, err)
				return
			}
		}
		mode := "optimize"
		if str(body["mode"]) == "repair" {
			mode = "repair"
		}
		result, err := dbx.OptimizeTables(app.Pool, toSSH(server), name, table, mode)
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = app.Store.AppendLog(map[string]any{
			"type": "db.optimize", "serverId": server.ID,
			"target": ternary(table != "", table, name),
			"detail": fmt.Sprintf("%v %v 张表", result["mode"], result["count"]), "result": "ok",
		})
		writeJSON(w, 200, result)
	})
}

// 文件权限只接受 3-4 位八进制（755 / 644 / 4755）
var octalModeRe = regexp.MustCompile(`^[0-7]{3,4}$`)

var protectedPaths = map[string]bool{
	"/": true, "/etc": true, "/usr": true, "/var": true, "/bin": true,
	"/sbin": true, "/boot": true, "/lib": true, "/root": true, "/home": true,
}

// ---------------------------------------------------------------- FTP 参数校验

var (
	ftpNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)
	ipv4Re    = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
)

// ---------------------------------------------------------------- 维护参数校验

var (
	phpVerRe  = regexp.MustCompile(`^[78]\.[0-9]$`)
	cronSched = regexp.MustCompile(`^[0-9*/,\-]+$`)
)

func assertPHPVer(v any) (string, error) {
	s := strings.TrimSpace(str(v))
	if !phpVerRe.MatchString(s) {
		return "", badRequest("PHP 版本要写成 8.3 这种形式")
	}
	return s, nil
}

// validateCronSchedule 与 scripts/cron.sh 的校验保持一致，早点报错省一次上传
func validateCronSchedule(v any) (string, error) {
	s := strings.TrimSpace(str(v))
	if s == "" {
		return "", badRequest("执行周期不能为空")
	}
	if strings.HasPrefix(s, "@") {
		switch s {
		case "@reboot", "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly":
			return s, nil
		}
		return "", badRequest("不支持的周期：" + s)
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return "", badRequest("周期要写 5 段：分 时 日 月 周（例如 0 3 * * *）")
	}
	for _, f := range fields {
		if !cronSched.MatchString(f) {
			return "", badRequest("周期里的「" + f + "」不是合法写法")
		}
	}
	return strings.Join(fields, " "), nil
}

func validateCronCommand(v any) (string, error) {
	s := strings.TrimSpace(str(v))
	if s == "" {
		return "", badRequest("命令不能为空")
	}
	if len(s) > 1000 {
		return "", badRequest("命令太长（最多 1000 字）")
	}
	if strings.ContainsAny(s, "\n\r") {
		return "", badRequest("命令里不能有换行，多条命令请用 && 或 ; 连接")
	}
	return s, nil
}

// panelLineFilter 过滤脚本末尾的 PANEL_ 状态行：它们只给程序看，不该出现在抽屉里
type panelLineFilter struct {
	push func(string)
	buf  string
}

func (f *panelLineFilter) Write(text string) {
	f.buf += text
	for {
		idx := strings.IndexByte(f.buf, '\n')
		if idx < 0 {
			return
		}
		line := f.buf[:idx+1]
		f.buf = f.buf[idx+1:]
		if strings.HasPrefix(strings.TrimSpace(line), "PANEL_") {
			continue
		}
		f.push(line)
	}
}

func (f *panelLineFilter) Flush() {
	if f.buf != "" && !strings.HasPrefix(strings.TrimSpace(f.buf), "PANEL_") {
		f.push(f.buf)
	}
	f.buf = ""
}

func cleanScriptLog(out string) string { return ftpLogText(out) }

// assertLogPath 与 scripts/logs.sh 的白名单一致：只允许动这三类日志，
// 面板能清空日志，但不能变成任意文件截断的入口
func assertLogPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return badRequest("日志路径必须是绝对路径")
	}
	if strings.Contains(p, "..") {
		return badRequest("路径里不能出现 ..")
	}
	switch {
	case strings.HasPrefix(p, "/var/log/nginx/"),
		strings.HasPrefix(p, "/var/log/apache2/"),
		strings.HasPrefix(p, "/var/log/php-fpm-"),
		strings.Contains(p, "/wp-content/debug.log"):
		return nil
	}
	return badRequest("只允许操作 nginx / apache / php-fpm / WordPress 调试日志")
}

func ftpParams(b map[string]any) ops.FTPParams {
	return ops.FTPParams{
		PasvMin:  intOf(b["pasvMin"]),
		PasvMax:  intOf(b["pasvMax"]),
		PasvAddr: strings.TrimSpace(str(b["pasvAddr"])),
	}
}

func validateFTPConfig(b map[string]any) error {
	p := ftpParams(b)
	// 安装时允许不传，脚本会自己探测公网地址、用默认端口段
	if p.PasvMin == 0 && p.PasvMax == 0 && p.PasvAddr == "" {
		return nil
	}
	if p.PasvMin < 1024 || p.PasvMin > 65535 || p.PasvMax < 1024 || p.PasvMax > 65535 {
		return badRequest("被动端口要在 1024-65535 之间")
	}
	if p.PasvMin > p.PasvMax {
		return badRequest("被动端口范围写反了")
	}
	if p.PasvAddr != "" && !ipv4Re.MatchString(p.PasvAddr) {
		return badRequest("公网地址要填 IPv4，例如 203.0.113.10")
	}
	return nil
}

func assertFTPDir(v string) (string, error) {
	d := strings.TrimRight(strings.TrimSpace(v), "/")
	if d == "" || !strings.HasPrefix(d, "/") {
		return "", badRequest("目录必须是绝对路径，例如 /var/www/example.com")
	}
	if strings.Contains(d, "..") {
		return "", badRequest("目录里不能出现 ..")
	}
	if protectedPaths[d] {
		return "", badRequest("该目录受保护，不能作为 FTP 根目录")
	}
	if len(strings.Split(d, "/")) < 3 {
		return "", badRequest("请指定具体的站点目录，不要用 /var 这类上层目录")
	}
	return d, nil
}

// ftpLogText 取脚本里给人看的日志行（去掉 PANEL_ 数据行），用于前端展示自检结果。
func ftpLogText(out string) string {
	lines := []string{}
	for _, line := range strings.Split(ops.StripANSI(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "PANEL_") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func assertFTPPassword(pw string) error {
	if len(pw) < 6 {
		return badRequest("密码至少 6 位")
	}
	if len(pw) > 128 {
		return badRequest("密码不能超过 128 位")
	}
	if strings.ContainsAny(pw, "\n\r") {
		return badRequest("密码不能包含换行")
	}
	return nil
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		if t != "" {
			if p, err := strconv.Atoi(t); err == nil {
				return p
			}
		}
	}
	return 0
}

func assertEngineIs(b map[string]any, want string) bool {
	e, _ := assertDbEngine(b["dbEngine"])
	return e == want
}

// ---------------------------------------------------------------- 静态资源与分发

func (app *App) serveStatic(w http.ResponseWriter, r *http.Request) {
	urlPath, err := url.PathUnescape(r.URL.Path)
	if err != nil {
		urlPath = r.URL.Path
	}
	if urlPath == "/" {
		urlPath = "/index.html"
	}
	name := strings.TrimPrefix(pathClean(urlPath), "/")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(app.Public, "public/"+name)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(404)
		_, _ = w.Write([]byte("页面不存在"))
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	ct := mimeTypes[ext]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func pathClean(p string) string {
	// 防目录穿越：只留文件名部分里的斜杠结构
	parts := strings.Split(strings.Trim(p, "/"), "/")
	var out []string
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, part)
	}
	return "/" + strings.Join(out, "/")
}

func (app *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Println("panic:", rec)
			if !headerWritten(w) {
				writeJSON(w, 500, map[string]string{"error": "服务器内部错误"})
			}
		}
	}()

	path := r.URL.Path
	if !strings.HasPrefix(path, "/api/") {
		app.serveStatic(w, r)
		return
	}
	// 插件自带的界面资源在运行时才落地，不走 embed FS
	if strings.HasPrefix(path, "/api/plugin-assets/") {
		app.servePluginAsset(w, r)
		return
	}

	// 防跨站写入：非 GET 必须带 x-panel: 1
	isRead := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !isRead && path != "/api/ping" && r.Header.Get("x-panel") != "1" {
		writeJSON(w, 403, map[string]string{"error": "缺少请求标识，请刷新页面重试"})
		return
	}

	segs := strings.Split(strings.Trim(path, "/"), "/")
	for _, rt := range routes {
		if rt.method != r.Method || len(rt.segments) != len(segs) {
			continue
		}
		params := map[string]string{}
		matched := true
		for i, seg := range rt.segments {
			if strings.HasPrefix(seg, ":") {
				decoded, err := url.PathUnescape(segs[i])
				if err != nil {
					decoded = segs[i]
				}
				params[seg[1:]] = decoded
			} else if seg != segs[i] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		rt.handler(app, w, r, params)
		return
	}

	writeJSON(w, 404, map[string]string{"error": fmt.Sprintf("接口不存在：%s %s", r.Method, path)})
}

func headerWritten(w http.ResponseWriter) bool {
	// ResponseWriter 没有直接暴露；用类型断言近似判断
	type flusher interface{ Written() bool }
	if f, ok := w.(flusher); ok {
		return f.Written()
	}
	return false
}

// NewApp 打开数据目录并组装依赖。数据目录的解析交给调用方（main），
// 这样双击启动时可以先决定用哪份档案再开服务。
func NewApp(dataDir string, public, scripts embed.FS) (*App, error) {
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, err
	}
	app := &App{
		Store:   st,
		Pool:    sshc.NewPool(),
		Runner:  &ops.Runner{Scripts: scripts, Paths: st.Paths},
		Public:  public,
		Version: Version,
		DataDir: dataDir,
		Updater: updater.New(dataDir, Version),
		Plugins: plugins.NewManager(dataDir),
		stopCh:  make(chan struct{}),
	}
	// Runner 与路由共享同一个连接池
	app.Runner.Pool = app.Pool
	return app, nil
}

// DataDir 返回实际使用的数据目录。
func (app *App) DataDirPath() string { return app.DataDir }

// Serve 在已就绪的 listener 上提供服务（阻塞）。
func (app *App) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: app, ReadHeaderTimeout: 0}
	app.srv = srv
	go func() {
		<-app.stopCh
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	err := srv.Serve(ln)
	app.Pool.CloseAll()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// RequestStop 通知服务退出（/api/shutdown 与 -stop 用）。
func (app *App) RequestStop() {
	app.stopOnce.Do(func() { close(app.stopCh) })
}

// Done 在服务被要求退出时关闭。
func (app *App) Done() <-chan struct{} { return app.stopCh }

// 保留 base64 引用（脚本上传路径生成时可能用到）
var _ = base64.StdEncoding
