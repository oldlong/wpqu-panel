package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"wpqu-panel/internal/ops"
	"wpqu-panel/internal/plugins"
	"wpqu-panel/internal/sshc"
	"wpqu-panel/internal/version"
)

// 插件市场 + 插件安装。安装走 ndjson 流，与建站那些操作保持同一套前端渲染。
func init() {
	// 目录：市场清单 + 本机已装 + 已保存的授权
	route("GET", "/api/plugins", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		out := map[string]any{
			"installed": app.Plugins.Installed(),
			"source":    app.Plugins.Base,
			"panel":     app.Version,
			"platform":  runtime.GOOS + "/" + runtime.GOARCH,
		}

		if cat, err := app.Plugins.Catalog(); err != nil {
			out["catalog"] = []any{}
			out["error"] = err.Error()
		} else {
			out["catalog"] = cat.Plugins
			out["updated"] = cat.Updated
		}

		// 已保存的授权码（只回显摘要，不回显原文）
		lics := map[string]any{}
		for id := range app.Plugins.Installed() {
			if lic, err := app.Plugins.License(id); err == nil {
				lics[id] = map[string]any{"name": lic.Name, "expire": lic.ExpireText(), "seats": lic.Seats}
			}
		}
		out["licenses"] = lics

		// 已装的 UI 插件变成页签
		tabs := []map[string]any{}
		installed := app.Plugins.Installed()
		if cat, err := app.Plugins.Catalog(); err == nil {
			for _, mf := range cat.Plugins {
				if mf.UI == nil || mf.UI.Entry == "" {
					continue
				}
				if _, ok := installed[mf.ID]; !ok {
					continue
				}
				tabs = append(tabs, map[string]any{
					"id":    mf.ID,
					"label": mf.UI.TabLabel,
					"name":  mf.Name,
					"icon":  mf.Icon,
					"entry": "/api/plugin-assets/" + mf.ID + "/" + strings.TrimPrefix(mf.UI.Entry, "/"),
				})
			}
		}
		out["uiTabs"] = tabs
		writeJSON(w, 200, out)
	})

	// 某台服务器上的安装状态（一次 SSH 批量检测）
	route("GET", "/api/servers/:id/plugins/status", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		server, err := app.getServerOrThrow(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		cat, err := app.Plugins.Catalog()
		if err != nil {
			writeErr(w, err)
			return
		}
		// 只检测已本地解压过的插件，没装的没必要跑命令
		installed := app.Plugins.Installed()
		var list []plugins.Manifest
		for _, mf := range cat.Plugins {
			if _, ok := installed[mf.ID]; ok {
				list = append(list, mf)
			}
		}
		if len(list) == 0 {
			writeJSON(w, 200, map[string]any{"status": map[string]any{}})
			return
		}
		res, err := app.Pool.Exec(toSSH(server), plugins.StatusScript(list), &sshc.ExecOptions{Timeout: 60 * time.Second})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"status": plugins.ParseStatus(res.Stdout)})
	})

	// 保存授权码：先本地验签，通过才落盘
	route("POST", "/api/plugins/:id/license", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		pid := p["id"]
		if !plugins.ValidID(pid) {
			writeErr(w, badRequest("插件 id 不合法"))
			return
		}
		body, err := readJSON(r, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		code := strings.TrimSpace(str(body["key"]))
		lic, err := app.Plugins.SaveLicense(pid, code)
		if err != nil {
			writeErr(w, badRequest(err.Error()))
			return
		}
		writeJSON(w, 200, map[string]any{
			"ok": true, "product": lic.Product, "name": lic.Name,
			"expire": lic.ExpireText(), "seats": lic.Seats,
		})
	})

	route("DELETE", "/api/plugins/:id/license", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		app.Plugins.ClearLicense(p["id"])
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// 移除本地插件包（不动服务器）
	route("DELETE", "/api/plugins/:id", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		if err := app.Plugins.Remove(p["id"]); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// 界面类插件：只装到面板本地，不碰服务器
	route("POST", "/api/plugins/:id/install", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		mf, err := app.Plugins.Find(p["id"])
		if err != nil {
			writeErr(w, err)
			return
		}
		if mf.UI == nil || mf.UI.Entry == "" {
			writeErr(w, badRequest("这个插件要装到服务器上，请先选中一台服务器"))
			return
		}
		if mf.Requires.License {
			if _, err := app.Plugins.License(mf.ID); err != nil {
				writeErr(w, badRequest("需要有效授权码："+err.Error()))
				return
			}
		}
		if _, err := app.Plugins.Ensure(*mf); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"ok":    true,
			"entry": "/api/plugin-assets/" + mf.ID + "/" + strings.TrimPrefix(mf.UI.Entry, "/"),
			"label": mf.UI.TabLabel,
		})
	})

	route("POST", "/api/servers/:id/plugins/:pid/install", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		handleInstall(app, w, r, p["id"], p["pid"])
	})

	route("POST", "/api/servers/:id/plugins/:pid/uninstall", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		handleUninstall(app, w, r, p["id"], p["pid"])
	})
}

// ---------------------------------------------------------------- 安装

func handleInstall(app *App, w http.ResponseWriter, r *http.Request, sid, pid string) {
	server, err := app.getServerOrThrow(sid)
	if err != nil {
		writeErr(w, err)
		return
	}
	body, err := readJSON(r, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	mf, err := app.Plugins.Find(pid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if mf.Install == nil || len(mf.Install.Steps) == 0 {
		writeErr(w, badRequest("该插件没有服务器安装配方"))
		return
	}
	if mf.Requires.MinPanel != "" && version.IsNewer(mf.Requires.MinPanel, app.Version) {
		writeErr(w, badRequest(fmt.Sprintf("该插件需要面板 v%s 及以上，当前 v%s", mf.Requires.MinPanel, app.Version)))
		return
	}
	// 收费插件：本地必须有有效授权码
	if mf.Requires.License {
		if _, err := app.Plugins.License(mf.ID); err != nil {
			writeErr(w, badRequest("需要有效授权码："+err.Error()))
			return
		}
	}

	// 目标服务器的系统必须匹配（插件配方全是 Linux 包管理命令）
	uni, err := app.Pool.Exec(toSSH(server), "uname -s", &sshc.ExecOptions{Timeout: 20 * time.Second})
	if err != nil {
		writeErr(w, err)
		return
	}
	remoteOS := strings.ToLower(strings.TrimSpace(ops.StripANSI(uni.Stdout)))
	if len(mf.Requires.OS) > 0 && !containsFold(mf.Requires.OS, remoteOS) {
		writeErr(w, badRequest(fmt.Sprintf("该插件只支持 %s，当前服务器是 %s", strings.Join(mf.Requires.OS, " / "), remoteOS)))
		return
	}

	params := map[string]string{}
	if raw, ok := body["params"].(map[string]any); ok {
		for k, v := range raw {
			k = strings.ToUpper(strings.TrimSpace(k))
			if k == "" || !validEnvKey(k) {
				continue
			}
			params["WPQU_"+k] = str(v)
		}
	}

	sw := startStream(w)
	started := time.Now()
	sw.event(map[string]any{"type": "start", "message": "准备安装 " + pickText(mf.Name)})
	push := func(text string) {
		sw.event(map[string]any{"type": "out", "text": ops.StripANSI(text)})
	}

	dir, err := app.Plugins.Ensure(*mf)
	if err != nil {
		sw.event(map[string]any{"type": "error", "message": err.Error()})
		_ = app.Store.AppendLog(map[string]any{
			"type": "plugin", "serverId": server.ID, "target": server.Host,
			"detail": "安装插件 " + mf.ID, "result": "error", "error": err.Error(),
		})
		return
	}

	total := len(mf.Install.Steps)
	for i, step := range mf.Install.Steps {
		title := pickText(step.Title)
		if title == "" {
			title = step.File
		}
		sw.event(map[string]any{"type": "step", "index": i + 1, "total": total, "title": title})

		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(step.File)))
		if err != nil {
			sw.event(map[string]any{"type": "error", "message": "插件包里缺少脚本：" + step.File})
			return
		}
		res, err := app.Runner.RunScriptText(toSSH(server), step.File, string(content), params, push, 40*time.Minute)
		if err != nil {
			sw.event(map[string]any{"type": "error", "message": err.Error()})
			_ = app.Store.AppendLog(map[string]any{
				"type": "plugin", "serverId": server.ID, "target": server.Host,
				"detail": "安装插件 " + mf.ID + "（" + title + "）", "result": "error", "error": err.Error(),
			})
			return
		}
		if res.Code != 0 {
			msg := fmt.Sprintf("第 %d 步「%s」失败（退出码 %d）", i+1, title, res.Code)
			sw.event(map[string]any{"type": "error", "message": msg})
			_ = app.Store.AppendLog(map[string]any{
				"type": "plugin", "serverId": server.ID, "target": server.Host,
				"detail": "安装插件 " + mf.ID + "（" + title + "）", "result": "fail",
				"durationMs": time.Since(started).Milliseconds(),
			})
			return
		}
	}

	sw.event(map[string]any{
		"type": "end", "ok": true,
		"durationMs": time.Since(started).Milliseconds(),
		"message":    "安装完成",
	})
	_ = app.Store.AppendLog(map[string]any{
		"type": "plugin", "serverId": server.ID, "target": server.Host,
		"detail": "安装插件 " + mf.ID + " v" + mf.Version, "result": "ok",
		"durationMs": time.Since(started).Milliseconds(),
	})
}

// ---------------------------------------------------------------- 卸载

func handleUninstall(app *App, w http.ResponseWriter, r *http.Request, sid, pid string) {
	server, err := app.getServerOrThrow(sid)
	if err != nil {
		writeErr(w, err)
		return
	}
	mf, err := app.Plugins.Find(pid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if mf.Install == nil || len(mf.Install.Uninstall) == 0 {
		writeErr(w, badRequest("该插件没有提供卸载脚本，请在服务器上手动处理"))
		return
	}

	sw := startStream(w)
	started := time.Now()
	sw.event(map[string]any{"type": "start", "message": "正在卸载 " + pickText(mf.Name)})
	push := func(text string) { sw.event(map[string]any{"type": "out", "text": ops.StripANSI(text)}) }

	dir := app.Plugins.LocalPath(mf.ID)
	for i, step := range mf.Install.Uninstall {
		title := pickText(step.Title)
		if title == "" {
			title = step.File
		}
		sw.event(map[string]any{"type": "step", "index": i + 1, "total": len(mf.Install.Uninstall), "title": title})
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(step.File)))
		if err != nil {
			sw.event(map[string]any{"type": "error", "message": "插件包里缺少脚本：" + step.File})
			return
		}
		res, err := app.Runner.RunScriptText(toSSH(server), step.File, string(content), nil, push, 20*time.Minute)
		if err != nil {
			sw.event(map[string]any{"type": "error", "message": err.Error()})
			return
		}
		if res.Code != 0 {
			sw.event(map[string]any{"type": "error", "message": fmt.Sprintf("第 %d 步「%s」失败（退出码 %d）", i+1, title, res.Code)})
			return
		}
	}
	sw.event(map[string]any{"type": "end", "ok": true, "message": "已卸载", "durationMs": time.Since(started).Milliseconds()})
	_ = app.Store.AppendLog(map[string]any{
		"type": "plugin", "serverId": server.ID, "target": server.Host,
		"detail": "卸载插件 " + mf.ID, "result": "ok",
	})
}

// ---------------------------------------------------------------- 静态资源

// servePluginAsset 提供已安装 UI 插件的文件（/api/plugin-assets/<id>/...）。
// 面板的静态资源是编译期嵌进去的，插件不行 —— 它们运行时才落地，只能走这里。
func (app *App) servePluginAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, 405, map[string]string{"error": "不支持的方法"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/plugin-assets/")
	id := rest
	rel := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id, rel = rest[:i], rest[i+1:]
	}
	data, err := app.Plugins.ReadFile(id, rel)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "资源不存在"})
		return
	}
	ct := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".js", ".mjs":
		ct = "application/javascript; charset=utf-8"
	case ".css":
		ct = "text/css; charset=utf-8"
	case ".json":
		ct = "application/json; charset=utf-8"
	case ".svg":
		ct = "image/svg+xml"
	case ".png":
		ct = "image/png"
	case ".woff2":
		ct = "font/woff2"
	}
	w.Header().Set("Content-Type", ct)
	// 插件可能随版本更新，禁用强缓存避免看到旧代码
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------- 小工具

// pickText 取多语言文案：优先中文，其次英文，最后任意一个非空的值。
// 面板界面本身是中英双语，插件文案允许只写一种。
func pickText(m map[string]string) string {
	if m == nil {
		return ""
	}
	for _, k := range []string{"zh-CN", "zh", "en"} {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	for _, v := range m {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// validEnvKey 只允许 A-Z0-9_，防止参数名被拼进 env 前缀后出问题。
func validEnvKey(k string) bool {
	for _, c := range k {
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return true
}
