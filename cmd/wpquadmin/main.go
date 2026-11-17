// wpquadmin：panel.wpqu.com 的管理后台。
//
// 它做两件事：改分发站的内容（插件清单、官网页面、升级发布），
// 以及按插件签发离线授权码。因为要读私钥，默认只监听 127.0.0.1。
//
// 设计成双击就能用：不带任何参数运行时，配置放在 exe 旁边，分发站目录
// 自动从 exe 位置往上找，口令没有就生成一个，起来之后自动开浏览器。
//
//	./wpquadmin                      # 日常启动（双击等价）
//	./wpquadmin -init                # 只生成/重置口令，不起服务
//	./wpquadmin -check               # 只做自检，不起服务（部署脚本用）
//	./wpquadmin -no-browser          # 不自动开浏览器
//	./wpquadmin -listen 127.0.0.1:7790 -site ../site -keys ../site/.keys
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"wpqu-panel/internal/admin"
	"wpqu-panel/internal/signing"
)

func main() {
	cfgFlag := flag.String("config", "", "配置文件路径，默认在 exe 旁边（admin.json）")
	listen := flag.String("listen", "", "监听地址，覆盖配置")
	site := flag.String("site", "", "分发站目录，覆盖配置")
	src := flag.String("src", "", "插件源码目录，覆盖配置")
	dist := flag.String("dist", "", "构建产物目录，覆盖配置")
	keys := flag.String("keys", "", "私钥目录，覆盖配置")
	data := flag.String("data", "", "数据目录，覆盖配置")
	initMode := flag.Bool("init", false, "只初始化：生成口令文件后退出（已存在则不改动）")
	password := flag.String("password", "", "配合 -init 指定口令，留空则随机生成")
	force := flag.Bool("force", false, "配合 -init：覆盖已存在的口令")
	check := flag.Bool("check", false, "只做自检，不起服务")
	noBrowser := flag.Bool("no-browser", false, "启动后不自动打开浏览器")
	flag.Parse()

	exeDir := exeDirPath()

	// 默认把配置放在 exe 旁边：双击时工作目录虽然是 exe 目录，
	// 但从快捷方式、计划任务启动时未必是，靠 CWD 找配置迟早出问题。
	cfgPath := *cfgFlag
	if cfgPath == "" {
		cfgPath = filepath.Join(exeDir, "admin.json")
	}
	cfg, created, err := admin.LoadConfig(cfgPath)
	if err != nil {
		fatal("%v", err)
	}

	// 命令行覆盖仅对本次进程生效：除非是 -init / 首次运行，否则不写回配置文件 ——
	// 免得调试一次就把正式路径改了。
	var keep admin.Keep
	if *site != "" {
		keep |= admin.KeepSite
	}
	if *src != "" {
		keep |= admin.KeepSrc
	}
	if *dist != "" {
		keep |= admin.KeepDist
	}
	if *keys != "" {
		keep |= admin.KeepKeys
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	for _, ov := range []struct {
		v   string
		dst *string
	}{{*site, &cfg.SiteDir}, {*src, &cfg.SrcDir}, {*dist, &cfg.DistDir},
		{*keys, &cfg.KeysDir}, {*data, &cfg.DataDir}} {
		if ov.v == "" {
			continue
		}
		abs, err := filepath.Abs(ov.v)
		if err != nil {
			fatal("路径 %s 不合法：%v", ov.v, err)
		}
		*ov.dst = abs
	}

	// 配置里的目录对不上时按 exe 位置重找一套，免得双击时 ../site 指到 dist/site。
	// 命令行显式指定的不动。
	notes := cfg.Locate(exeDir, keep)

	// 第一次运行、显式 -init、或者刚纠正过路径，都把「实际生效的配置」落盘 ——
	// 否则下次启动又按同一个错路径解析，用户会以为改了没用。
	// 已有配置且这次没纠正时不动它，临时 -site 就不会被固化。
	if created || *initMode || len(notes) > 0 {
		if err := cfg.Save(cfgPath); err != nil {
			fatal("写配置文件失败：%v", err)
		}
	}

	authPath := dataFile(cfg, "auth.json")
	if *initMode {
		// 初始化是一次性动作，做完就退出：让它顺手把服务起起来，
		// 在脚本里会表现为「命令卡住不返回」，很难判断到底成没成。
		doInit(cfg, cfgPath, authPath, *password, *force, notes)
		return
	}
	if *check {
		os.Exit(runCheck(cfg))
	}

	// 分发站目录找不到就别往下走了：后面每一步都在读它，报错会更难懂。
	if !admin.IsSiteDir(cfg.SiteDir) {
		fatal(`没找到分发站目录。

  当前配置：%s
  配置文件：%s

把 wpquadmin 放在仓库里（site 的上一级或上两级都行），
或者用 -site 指向 panel.wpqu.com 的站点根目录。`, cfg.SiteDir, cfgPath)
	}

	// 已经有一个实例在跑？把浏览器指过去就算了，不要起第二份去抢同一批台账文件。
	if url := probeRunning(cfg); url != "" {
		fmt.Printf("已经有一个实例在运行：%s\n要重启就先关掉那个窗口（或在里面按 Ctrl+C）。\n", url)
		if !*noBrowser {
			fmt.Println("已为你打开浏览器。")
			openBrowser(url)
		}
		return
	}

	// 双击场景没有「先跑 -init」这一步，口令不存在就顺手生成一个。
	freshPwd := ""
	if _, err := os.Stat(authPath); err != nil {
		freshPwd = admin.RandomPassword(16)
		if err := saveAuth(cfg, freshPwd); err != nil {
			fatal("写口令文件失败：%v", err)
		}
	}

	srv, err := admin.New(cfg)
	if err != nil {
		fatal("%v", err)
	}

	ln, addr, err := listenWithFallback(cfg.Listen, 10)
	if err != nil {
		fatal("启动失败：%v", err)
	}
	if addr != cfg.Listen {
		fmt.Printf("端口 %s 已被占用，这次改用 %s\n", cfg.Listen, addr)
	}

	banner(cfg, addr, notes)
	if freshPwd != "" {
		showPassword(freshPwd, authPath)
	}
	writeRun(cfg, addr)
	defer os.Remove(dataFile(cfg, "run.json"))

	url := "http://" + hostForURL(addr) + "/"
	if !*noBrowser {
		if err := openBrowser(url); err != nil {
			fmt.Printf("（没能自动打开浏览器，请手动访问 %s）\n", url)
		}
	}
	fmt.Println("按 Ctrl+C 停止。")

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("服务出错：%v", err)
		}
	}()

	// 等 Ctrl+C 或 systemd stop，给在途请求 5 秒收尾
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("正在退出…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

// ---------------------------------------------------------------- 启动辅助

// exeDirPath 返回可执行文件所在目录。`go run` 时返回临时目录，没有意义，
// 这种情况退回当前工作目录，开发时行为才符合直觉。
func exeDirPath() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if strings.Contains(p, "go-build") {
		return "."
	}
	return filepath.Dir(p)
}

// listenWithFallback 绑定端口，被占用时往后顺延几个。
// 双击的人不该因为 7790 被别的程序占了就看不到界面。
func listenWithFallback(addr string, tries int) (net.Listener, string, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("监听地址 %q 不合法，应形如 127.0.0.1:7790：%w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, "", fmt.Errorf("监听地址 %q 的端口不是数字", addr)
	}
	var lastErr error
	for i := 0; i < tries; i++ {
		a := net.JoinHostPort(host, strconv.Itoa(port+i))
		ln, err := net.Listen("tcp", a)
		if err == nil {
			return ln, a, nil
		}
		lastErr = err
		if !isAddrInUse(err) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("%d 个端口都被占用（%s 起）：%w", tries, addr, lastErr)
}

func isAddrInUse(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) {
		return strings.Contains(oe.Err.Error(), "Only one usage") ||
			strings.Contains(oe.Err.Error(), "address already in use")
	}
	return strings.Contains(err.Error(), "Only one usage") ||
		strings.Contains(err.Error(), "address already in use")
}

// hostForURL 把监听地址转成能直接往浏览器里填的主机名。
func hostForURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

type runInfo struct {
	Listen    string `json:"listen"`
	Pid       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
}

func writeRun(cfg admin.Config, addr string) {
	body, _ := json.MarshalIndent(runInfo{
		Listen: addr, Pid: os.Getpid(), StartedAt: time.Now().Format(time.RFC3339),
	}, "", "  ")
	_ = os.WriteFile(dataFile(cfg, "run.json"), append(body, '\n'), 0o600)
}

// probeRunning 判断是否已经有实例在跑。认 /api/ping 而不是「端口通就算」，
// 免得把别的程序当成自己。
func probeRunning(cfg admin.Config) string {
	var cands []string
	if raw, err := os.ReadFile(dataFile(cfg, "run.json")); err == nil {
		var ri runInfo
		if json.Unmarshal(raw, &ri) == nil && ri.Listen != "" {
			cands = append(cands, ri.Listen)
		}
	}
	cands = append(cands, cfg.Listen)

	seen := map[string]bool{}
	for _, a := range cands {
		if seen[a] {
			continue
		}
		seen[a] = true
		base := "http://" + hostForURL(a)
		cl := &http.Client{Timeout: 700 * time.Millisecond}
		resp, err := cl.Get(base + "/api/ping")
		if err != nil {
			continue
		}
		var body struct {
			App string `json:"app"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if body.App == "wpquadmin" {
			return base + "/"
		}
	}
	return ""
}

// ---------------------------------------------------------------- 口令

func saveAuth(cfg admin.Config, password string) error {
	a, err := admin.NewAuth("admin", password)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	return a.Save(dataFile(cfg, "auth.json"))
}

// showPassword 把新口令同时放到窗口和弹窗里：双击启动的人错过窗口就真找不回来了。
func showPassword(password, authPath string) {
	fmt.Printf(`
── 已生成管理员口令 ──────────────────────────
  用户名：admin
  口令：  %s
  存于：  %s
这个口令只显示这一次，请立刻存进密码管理器。
忘了就运行：  wpquadmin -init -force -password 新口令
──────────────────────────────────────────────
`, password, authPath)
	if os.Getenv("WPQU_NO_POPUP") == "" {
		popupInfo("wpquadmin 已生成管理员口令",
			"用户名：admin\n口令："+password+
				"\n\n这个口令只显示这一次，请立刻存进密码管理器。\n"+
				"忘了可以运行 wpquadmin -init -force -password 新口令 重置。")
	}
}

func doInit(cfg admin.Config, cfgPath, authPath, password string, force bool, notes []string) {
	for _, n := range notes {
		fmt.Println("自动纠正路径 " + n)
	}
	if _, err := os.Stat(authPath); err == nil && !force {
		fmt.Printf("口令文件已存在，保持不变：%s\n（要重置请加 -force -password 新口令）\n\n直接运行即可启动：%s\n",
			authPath, os.Args[0])
		return
	}
	if password == "" {
		password = admin.RandomPassword(16)
	}
	if err := saveAuth(cfg, password); err != nil {
		fatal("写口令文件失败：%v", err)
	}
	fmt.Printf(`已初始化管理后台
  配置文件：%s
  口令文件：%s（0600，仅本人可读）
  用户名：  admin
  口令：    %s

这个口令只显示一次，请立刻存进密码管理器。忘了就用
  %s -init -force -password 新口令
重置。

下一步：直接运行 %s（或双击它），会自动打开浏览器。
`, cfgPath, authPath, password, os.Args[0], os.Args[0])
	if os.Getenv("WPQU_NO_POPUP") == "" {
		popupInfo("wpquadmin 已初始化", "用户名：admin\n口令："+password+
			"\n\n这个口令只显示这一次，请立刻存进密码管理器。")
	}
}

func dataFile(cfg admin.Config, name string) string {
	return filepath.Join(cfg.DataDir, name)
}

// ---------------------------------------------------------------- 自检与横幅

// runCheck 供部署脚本调用：密钥不匹配就别上线，否则签出的东西全是废的。
func runCheck(cfg admin.Config) int {
	fmt.Println("配置：")
	fmt.Printf("  监听    %s\n  站点    %s\n  插件源  %s\n  构建    %s\n  私钥    %s\n  数据    %s\n",
		cfg.Listen, cfg.SiteDir, cfg.SrcDir, cfg.DistDir, cfg.KeysDir, cfg.DataDir)

	fail := 0
	if !admin.IsSiteDir(cfg.SiteDir) {
		fmt.Printf("  ✗ 站点目录不像分发站：%s\n", cfg.SiteDir)
		fail++
	}
	expected := []struct {
		name, path, want string
	}{
		{"发布签名", filepath.Join(cfg.KeysDir, "release.key"), signing.ReleasePublicKey},
		{"授权签发", filepath.Join(cfg.KeysDir, "license.key"), signing.LicensePublicKey},
	}
	for _, k := range expected {
		pub, err := signing.PublicKeyOf(k.path)
		switch {
		case err != nil:
			fmt.Printf("  ✗ %s：%v\n", k.name, err)
			fail++
		case pub != k.want:
			fmt.Printf("  ✗ %s：公钥与面板内置常量不一致\n      实际 %s\n      面板 %s\n", k.name, pub, k.want)
			fail++
		default:
			fmt.Printf("  ✓ %s：公钥与面板一致\n", k.name)
		}
	}
	if _, err := os.Stat(dataFile(cfg, "auth.json")); err != nil {
		fmt.Println("  ✗ 还没有设置口令：正常启动一次会自动生成，或用 -init 指定")
		fail++
	}
	if fail > 0 {
		fmt.Println("\n自检未通过（" + fmt.Sprint(fail) + " 项）")
		return 1
	}
	fmt.Println("\n自检通过")
	return 0
}

func banner(cfg admin.Config, addr string, notes []string) {
	fmt.Printf("wpquadmin v%s\n", admin.Version)
	fmt.Printf("  地址    http://%s\n", hostForURL(addr))
	fmt.Printf("  站点    %s\n", cfg.SiteDir)
	fmt.Printf("  插件源  %s\n", cfg.SrcDir)
	fmt.Printf("  私钥    %s\n", cfg.KeysDir)
	fmt.Printf("  数据    %s\n", cfg.DataDir)
	for _, n := range notes {
		fmt.Printf("  · 自动纠正 %s\n", n)
	}

	// 非回环地址意味着同网段的人都能打开登录页，必须提示
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			fmt.Printf("\n⚠ 正在监听 %s，不是回环地址。这个后台能签发授权码、能替换升级包，\n"+
				"  请确认前面有 TLS 与访问控制（见 docs/管理后台说明.md）。\n", host)
		}
	}
	fmt.Println()
}

// fatal 出错时除了写 stderr，还弹一个窗口：双击启动的人看不到 stderr，
// 控制台窗口会在报错后立刻关掉，表现就是「双击没反应」。
// 脚本 / 自动化里设 WPQU_NO_POPUP=1 跳过弹窗。
func fatal(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintln(os.Stderr, "✗ "+msg)
	if os.Getenv("WPQU_NO_POPUP") == "" {
		popupError("wpquadmin 启动失败", msg)
	}
	os.Exit(1)
}
