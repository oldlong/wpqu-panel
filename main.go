// 静态资源与服务器脚本全部嵌进二进制，单文件即可运行。
// 双击启动即开网页（无控制台窗口）；再双击会复用已在运行的实例。
package main

import (
	"embed"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"wpqu-panel/internal/httpapi"
	"wpqu-panel/internal/updater"
)

//go:embed public
var publicFS embed.FS

//go:embed scripts
var scriptsFS embed.FS

func main() {
	port := flag.String("port", envOr("PORT", "7788"), "监听端口（被占用时自动向后顺延）")
	host := flag.String("host", envOr("HOST", "127.0.0.1"), "监听地址，默认只监听本机")
	dataFlag := flag.String("data", os.Getenv("PANEL_DATA"), "数据目录（默认 %APPDATA%\\wpqu-panel\\data）")
	noBrowser := flag.Bool("no-browser", false, "启动后不自动打开浏览器")
	stop := flag.Bool("stop", false, "停止正在运行的面板")
	flag.Parse()

	if *stop {
		stopRunning(*host, *port)
		return
	}

	dataDir, notes := resolveDataDir(*dataFlag)
	setupLog(dataDir)
	updater.CleanupOld() // 清掉上次升级留下的 .old

	log.Println("WordPress 服务器管理面板（Go 版）v" + httpapi.Version)
	for _, n := range notes {
		log.Println(n)
	}
	log.Println("数据目录：" + dataDir)

	// 端口上已经有本面板在跑
	if url, oldVer, oldPort, ok := findRunningPanel(*host, *port); ok {
		// 升级流程拉起的新进程带 WPQU_RESTARTED=1：此刻旧进程正准备退出，
		// 就算版本号一样也必须接管端口，否则「重启」会变成两个都退出
		restarted := os.Getenv("WPQU_RESTARTED") == "1"
		if !restarted && (oldVer == httpapi.Version || oldVer == "") {
			log.Println("检测到已有实例在运行：" + url)
			if !*noBrowser {
				if err := openBrowser(url); err != nil {
					log.Println("打开浏览器失败：" + err.Error())
					messageBox("面板已在运行", "请在浏览器打开：\n"+url)
				}
			}
			return
		}

		// 版本不同：多半是 exe 更新了，但旧进程还在跑。直接复用会让用户
		// 对着旧界面以为新功能没生效，所以这里把旧进程换掉再启动。
		log.Printf("端口 %s 上是旧版本 v%s，正在替换为 v%s", oldPort, oldVer, httpapi.Version)
		if !shutdownPanel(*host, oldPort) {
			fail("无法替换旧版本",
				fmt.Sprintf("端口 %s 上运行着 v%s 的面板，但它没有响应关闭请求。\n\n"+
					"请手动结束该进程后重新启动本程序，或用 -port 换一个端口。", oldPort, oldVer))
			return
		}
		if !waitPortFree(*host, oldPort) {
			log.Println("旧进程未在 5 秒内退出，将顺延到下一个端口启动")
		} else {
			log.Println("旧版本已退出")
		}
	}

	app, err := httpapi.NewApp(dataDir, publicFS, scriptsFS)
	if err != nil {
		fail("初始化失败", err.Error())
		return
	}

	ln, actualPort, err := listenWithFallback(*host, *port)
	if err != nil {
		fail("端口被占用", fmt.Sprintf("无法在 %s 的 %s 起监听。\n\n%v\n\n可以用 -port 指定其他端口。", *host, *port, err))
		return
	}

	url := browserURL(*host, actualPort)
	log.Println("服务地址：" + url)
	log.Println("提示：面板以 root 身份操作远程服务器，请勿把端口暴露到公网。")

	if !*noBrowser {
		go func() {
			// listener 已就绪，稍微延迟让首批连接更稳
			time.Sleep(150 * time.Millisecond)
			if err := openBrowser(url); err != nil {
				log.Println("打开浏览器失败：" + err.Error())
				messageBox("面板已启动", "请在浏览器打开：\n"+url)
			} else {
				log.Println("已打开默认浏览器")
			}
		}()
	}

	if *host != "127.0.0.1" && *host != "localhost" && *host != "::1" {
		log.Println("⚠ 面板正在监听非本地地址：" + *host)
		log.Println("  该面板以 root 身份操作远程服务器，暴露到公网极其危险。")
	}

	if err := app.Serve(ln); err != nil {
		fail("服务异常退出", err.Error())
		return
	}
	log.Println("面板已退出")
}

// fail 报告致命错误：控制台可见时直接打印，双击（无控制台）时弹窗。
func fail(title, detail string) {
	log.Println(title + "：" + detail)
	if !hasConsole() {
		messageBox(title, detail)
	}
}

func browserURL(host string, port string) string {
	h := host
	switch host {
	case "0.0.0.0", "::", "":
		h = "127.0.0.1"
	case "::1":
		h = "[::1]"
	}
	return "http://" + net.JoinHostPort(h, port)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------- 端口

// listenWithFallback 在 port..port+20 之间找第一个可用端口。
// 这样旧版本占着 7788 时，新版不会直接报错退出。
func listenWithFallback(host, port string) (net.Listener, string, error) {
	start, err := strconv.Atoi(port)
	if err != nil || start <= 0 || start > 65535 {
		return nil, "", fmt.Errorf("端口不合法：%s", port)
	}
	var lastErr error
	for p := start; p < start+20 && p <= 65535; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err == nil {
			return ln, strconv.Itoa(p), nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}

// findRunningPanel 探测端口段里有没有已在运行的本面板（impl=go），
// 顺便把对方的版本号带回来：exe 被新版本覆盖后，旧进程仍会占着端口，
// 这时如果直接复用，用户看到的永远是旧界面。
func findRunningPanel(host, port string) (url, version, portNum string, ok bool) {
	start, err := strconv.Atoi(port)
	if err != nil {
		return "", "", "", false
	}
	client := &http.Client{Timeout: 700 * time.Millisecond}
	for p := start; p < start+20 && p <= 65535; p++ {
		addr := net.JoinHostPort(probeHost(host), strconv.Itoa(p))
		resp, err := client.Get("http://" + addr + "/api/ping")
		if err != nil {
			continue
		}
		impl, ver := "", ""
		if m, ok := decodeJSONObject(resp); ok {
			impl, _ = m["impl"].(string)
			ver, _ = m["version"].(string)
		}
		resp.Body.Close()
		if impl == "go" {
			return browserURL(host, strconv.Itoa(p)), ver, strconv.Itoa(p), true
		}
	}
	return "", "", "", false
}

// shutdownPanel 请求指定端口上的面板退出。
func shutdownPanel(host, port string) bool {
	addr := net.JoinHostPort(probeHost(host), port)
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest("POST", "http://"+addr+"/api/shutdown", nil)
	if err != nil {
		return false
	}
	req.Header.Set("x-panel", "1")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	m, _ := decodeJSONObject(resp)
	resp.Body.Close()
	ok, _ := m["ok"].(bool)
	return ok
}

// waitPortFree 等旧进程真正放开端口，最多等 5 秒。
func waitPortFree(host, port string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
		if err == nil {
			ln.Close()
			return true
		}
		time.Sleep(120 * time.Millisecond)
	}
	return false
}

func stopRunning(host, port string) {
	start, err := strconv.Atoi(port)
	if err != nil {
		fail("参数错误", "端口不合法："+port)
		return
	}
	for p := start; p < start+20 && p <= 65535; p++ {
		if shutdownPanel(host, strconv.Itoa(p)) {
			log.Println("已停止运行在 " + strconv.Itoa(p) + " 的面板")
			if !hasConsole() {
				messageBox("面板已停止", "已关闭端口 "+strconv.Itoa(p)+" 上的面板进程。")
			}
			return
		}
	}
	log.Println("没找到正在运行的面板")
	if !hasConsole() {
		messageBox("没有运行中的面板", "未在 "+port+" 起连续 20 个端口上找到正在运行的面板。")
	}
}

func probeHost(host string) string {
	switch host {
	case "0.0.0.0", "::", "":
		return "127.0.0.1"
	}
	return host
}
