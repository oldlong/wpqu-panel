// 紧急修复：拉公告 / 检测服务器 / 一键执行修复。
//
// 命令一律来自签名清单，前端只传 fix id —— 这是这套接口最要紧的约束，
// 否则「一键修复」就变成了一个绕过命令白名单的任意执行入口。
package httpapi

import (
	"net/http"
	"strings"
	"time"

	"wpqu-panel/internal/security"
	"wpqu-panel/internal/sshc"
	"wpqu-panel/internal/store"
)

// GET /api/security/feed?refresh=1
func (app *App) securityFeed(w http.ResponseWriter, r *http.Request) {
	force := strings.TrimSpace(r.URL.Query().Get("refresh")) == "1"
	feed, err := app.Security.Feed(force)
	if err != nil {
		writeErr(w, err)
		return
	}
	usable := feed.Usable(app.Version)
	writeJSON(w, 200, map[string]any{
		"updated":   feed.Updated,
		"notices":   feed.Notices,
		"fixes":     usable,
		"source":    app.Security.Base,
		"fetchedAt": app.Security.FetchedAt().Format(time.RFC3339),
		"panel":     app.Version,
		// 页面上写死的入口：公告的完整版（人看的页面）在这里
		"page": strings.TrimRight(app.Security.Base, "/") + "/security.html",
	})
}

// POST /api/servers/:id/security/scan
func (app *App) securityScan(w http.ResponseWriter, r *http.Request, p map[string]string) {
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
	force := boolDefault(body["refresh"], false)
	feed, err := app.Security.Feed(force)
	if err != nil {
		writeErr(w, err)
		return
	}
	fixes := feed.Usable(app.Version)

	items := make([]security.Result, 0, len(fixes))
	if len(fixes) > 0 {
		script := security.ScanScript(fixes)
		// apt-get update 加上模拟安装，慢的时候十几秒，给足 5 分钟
		res, err := app.Runner.RunCommand(toSSH(server), script, "", 5*time.Minute, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		parsed := security.ParseScan(fixes, res.Stdout)
		for _, f := range fixes {
			if r, ok := parsed[f.ID]; ok {
				items = append(items, r)
			}
		}
	}
	affected := 0
	for _, it := range items {
		if it.Affected {
			affected++
		}
	}
	writeJSON(w, 200, map[string]any{
		"items":     items,
		"affected":  affected,
		"checkedAt": time.Now().Format(time.RFC3339),
		"updated":   feed.Updated,
		"panel":     app.Version,
	})
}

// POST /api/servers/:id/security/fix  {id}
func securityFix() streamCall {
	return streamCall{
		logType:    "security.fix",
		noDiagnose: true,
		describe: func(b map[string]any) string {
			id := strings.TrimSpace(str(b["id"]))
			return "紧急修复：" + id
		},
		validate: func(b map[string]any) error {
			if strings.TrimSpace(str(b["id"])) == "" {
				return badRequest("缺少修复编号")
			}
			return nil
		},
		run: func(app *App, server *store.ServerWithSecret, body map[string]any, push func(string)) (*sshc.Result, error) {
			id := strings.TrimSpace(str(body["id"]))
			fix, err := app.Security.Lookup(id)
			if err != nil {
				return nil, err
			}
			script := fix.Script()
			if strings.TrimSpace(script) == "" {
				return nil, badRequest("这条修复没有可用步骤")
			}
			push("即将执行：\n" + indentScript(script) + "\n")
			// 升级可能要装不少包，给满 10 分钟（RunCommand 的上限）
			return app.Runner.RunCommand(toSSH(server), script, "", 10*time.Minute, push)
		},
	}
}

func indentScript(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}
