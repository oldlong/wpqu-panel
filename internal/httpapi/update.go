package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"wpqu-panel/internal/updater"
)

// 面板自升级。只做「面板自己」这一件事 —— 服务器上的软件版本不在这里动。
func init() {
	route("GET", "/api/update/check", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		res, err := app.Updater.Check()
		if err != nil {
			// 检查失败不该是 500：网络不通是常态，前端要能把原因显示出来
			writeJSON(w, 200, map[string]any{
				"version":       app.Version,
				"canSelfUpdate": false,
				"source":        app.Updater.Base,
				"error":         err.Error(),
			})
			return
		}
		can, reason := updater.CanSelfUpdate()
		writeJSON(w, 200, map[string]any{
			"version":       app.Version,
			"canSelfUpdate": can,
			"reason":        reason,
			"source":        app.Updater.Base,
			"result":        res,
		})
	})

	route("POST", "/api/update/apply", func(app *App, w http.ResponseWriter, r *http.Request, p map[string]string) {
		// 不信任前端传来的 URL 和 SHA256：重新检查一遍，用服务端算出来的结果下载
		res, err := app.Updater.Check()
		if err != nil {
			writeErr(w, err)
			return
		}
		if !res.Available {
			writeErr(w, badRequest("没有可安装的更新："+res.Skipped))
			return
		}
		if ok, reason := updater.CanSelfUpdate(); !ok {
			writeErr(w, badRequest(reason))
			return
		}

		sw := startStream(w)
		sw.event(map[string]any{"type": "start", "message": fmt.Sprintf("正在升级到 v%s", res.Latest)})

		staged, err := app.Updater.Stage(res, func(pct float64, msg string) {
			sw.event(map[string]any{"type": "progress", "pct": pct, "message": msg})
		})
		if err != nil {
			updater.Log(app.DataDir, "升级到 v%s 失败：%v", res.Latest, err)
			sw.event(map[string]any{"type": "error", "message": err.Error()})
			return
		}

		sw.event(map[string]any{"type": "progress", "pct": 1, "message": "正在替换程序并重启…"})
		if err := updater.Restart(staged); err != nil {
			updater.Log(app.DataDir, "替换为 v%s 失败：%v", res.Latest, err)
			sw.event(map[string]any{"type": "error", "message": err.Error()})
			return
		}
		updater.Log(app.DataDir, "已升级到 v%s，进程即将重启", res.Latest)
		sw.event(map[string]any{
			"type": "end", "ok": true, "version": res.Latest,
			"message": "新版本已就位，面板正在重启",
		})

		// 给响应留出刷盘时间，再请求退出；退出后外部脚本会把新二进制换上去
		go func() {
			time.Sleep(700 * time.Millisecond)
			app.RequestStop()
		}()
	})
}
