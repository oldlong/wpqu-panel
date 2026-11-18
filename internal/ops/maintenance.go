package ops

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"wpqu-panel/internal/sshc"
)

// 维护栏目：PHP 版本、网站日志、定时任务、执行命令。
// 远端逻辑都在 scripts/php-manage.sh、logs.sh、cron.sh 里，这里只负责传参、解析和超时。

// ---------------------------------------------------------------- PHP 版本

// PHPVersionInfo 是一个已安装的 PHP 版本。
type PHPVersionInfo struct {
	Version    string `json:"version"`
	Running    bool   `json:"running"`    // php<v>-fpm 是否在跑
	Extensions int    `json:"extensions"` // 已启用扩展数
	Sites      int    `json:"sites"`      // 有多少站点在用
	DefaultCLI bool   `json:"defaultCli"` // 是否是 php 命令指向的版本
}

// PHPSiteInfo 是一个站点当前的 PHP 绑定。
type PHPSiteInfo struct {
	Domain  string `json:"domain"`
	Type    string `json:"type"`
	Version string `json:"version"` // 静态站为空
	Pool    string `json:"pool"`
	Root    string `json:"root"`
}

// PHPAvailable 是候选版本的可安装状态。
type PHPAvailable struct {
	Version string `json:"version"`
	State   string `json:"state"` // installed / ready / missing
}

// PHPStatus 是 PHP 页面的完整数据。
type PHPStatus struct {
	OS        string           `json:"os"`
	OSVersion string           `json:"osVersion"`
	Codename  string           `json:"codename"`
	CLI       string           `json:"cli"`
	CLIPath   string           `json:"cliPath"`
	HasPHP    bool             `json:"hasPhp"`
	ExtraRepo bool             `json:"extraRepo"` // 是否已加 Sury / ondrej 源
	Versions  []PHPVersionInfo `json:"versions"`
	Sites     []PHPSiteInfo    `json:"sites"`
	Available []PHPAvailable   `json:"available"`
}

// PHP 相关操作的超时：装/卸要跑 apt，其余是秒级
func phpTimeout(action string) time.Duration {
	switch action {
	case "install", "remove":
		return 12 * time.Minute
	case "repo":
		return 6 * time.Minute
	case "status":
		return 90 * time.Second
	}
	return 3 * time.Minute
}

// PHPStatus 读取远端 PHP 现状。
func (r *Runner) PHPStatus(server sshc.Server) (*PHPStatus, error) {
	res, err := r.RunUploadedScript(server, "php-manage.sh", map[string]string{"PHP_ACTION": "status"}, nil, phpTimeout("status"))
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("%s", errText(res, "读取 PHP 状态失败"))
	}
	return ParsePHPStatus(StripANSI(res.Stdout)), nil
}

// PHPAction 执行一次 PHP 改动，返回最新状态与脚本原始输出。
func (r *Runner) PHPAction(server sshc.Server, action, target, domain string, force bool, onData func(string)) (*PHPStatus, *sshc.Result, error) {
	env := map[string]string{"PHP_ACTION": action}
	if target != "" {
		env["PHP_TARGET"] = target
	}
	if domain != "" {
		env["PHP_DOMAIN"] = domain
	}
	if force {
		env["PHP_FORCE"] = "1"
	}
	res, err := r.RunUploadedScript(server, "php-manage.sh", env, onData, phpTimeout(action))
	if err != nil {
		return nil, nil, err
	}
	out := ""
	if res != nil {
		out = StripANSI(res.Stdout)
	}
	st := ParsePHPStatus(out)
	if res.Code != 0 {
		return st, res, fmt.Errorf("%s", errText(res, "PHP 操作失败"))
	}
	return st, res, nil
}

func errText(res *sshc.Result, fallback string) string {
	if res == nil {
		return fallback
	}
	all := StripANSI(res.Stdout + "\n" + res.Stderr)
	if msg := LastErrorLine(all); msg != "" {
		return msg
	}
	// 脚本没写 [err] 时，把最后一行有内容的输出带出来，否则前端只能看到一句干巴巴的「失败」
	lines := strings.Split(all, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "PANEL_") {
			continue
		}
		return line
	}
	return fallback
}

// ParsePHPStatus 解析脚本输出的 PANEL_ 键值。
func ParsePHPStatus(out string) *PHPStatus {
	st := &PHPStatus{
		Versions:  []PHPVersionInfo{},
		Sites:     []PHPSiteInfo{},
		Available: []PHPAvailable{},
	}
	siteVer := map[string]int{}

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "PANEL_") {
			continue
		}
		kv := strings.SplitN(strings.TrimPrefix(line, "PANEL_"), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := kv[0], kv[1]
		switch key {
		case "OS":
			parts := strings.Split(val, "|")
			st.OS = parts[0]
			if len(parts) > 1 {
				st.OSVersion = parts[1]
			}
			if len(parts) > 2 {
				st.Codename = parts[2]
			}
		case "CLI":
			st.CLI = val
		case "CLI_BIN":
			st.CLIPath = val
		case "NOPHP":
			st.HasPHP = val != "1"
		case "EXTRA_REPO":
			st.ExtraRepo = val == "1"
		case "VER":
			parts := strings.Split(val, "|")
			if len(parts) < 3 {
				continue
			}
			ext, _ := strconv.Atoi(parts[2])
			st.Versions = append(st.Versions, PHPVersionInfo{
				Version:    parts[0],
				Running:    parts[1] == "1",
				Extensions: ext,
			})
		case "SITE":
			parts := strings.Split(val, "|")
			if len(parts) < 5 {
				continue
			}
			ver := parts[2]
			if ver == "-" {
				ver = ""
			} else {
				siteVer[ver]++
			}
			pool := parts[3]
			if pool == "-" {
				pool = ""
			}
			st.Sites = append(st.Sites, PHPSiteInfo{
				Domain:  decodeB64(parts[0]),
				Type:    parts[1],
				Version: ver,
				Pool:    pool,
				Root:    decodeB64(parts[4]),
			})
		case "AVAIL":
			parts := strings.Split(val, "|")
			if len(parts) < 2 {
				continue
			}
			st.Available = append(st.Available, PHPAvailable{Version: parts[0], State: parts[1]})
		}
	}

	if len(st.Versions) > 0 {
		st.HasPHP = true
	}
	for i := range st.Versions {
		st.Versions[i].Sites = siteVer[st.Versions[i].Version]
		if st.CLI != "" && st.Versions[i].Version == st.CLI {
			st.Versions[i].DefaultCLI = true
		}
	}
	return st
}

func decodeB64(s string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return s
	}
	return string(raw)
}

// ---------------------------------------------------------------- 网站日志

// LogFile 是一个可查看的日志文件。
type LogFile struct {
	Domain  string `json:"domain"`
	Kind    string `json:"kind"` // access / error / php / debug / other / archive
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Mtime   int64  `json:"mtime"`
	Archive bool   `json:"archive"`
}

// Logs 列出站点相关的日志文件。
func (r *Runner) Logs(server sshc.Server) ([]LogFile, error) {
	res, err := r.RunUploadedScript(server, "logs.sh", map[string]string{"LOG_ACTION": "list"}, nil, 90*time.Second)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("%s", errText(res, "读取日志列表失败"))
	}
	out := StripANSI(res.Stdout)
	files := []LogFile{}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "PANEL_LOG=") {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(line, "PANEL_LOG="), "|")
		if len(parts) < 6 {
			continue
		}
		size, _ := strconv.ParseInt(parts[3], 10, 64)
		mtime, _ := strconv.ParseInt(parts[4], 10, 64)
		files = append(files, LogFile{
			Domain:  decodeB64(parts[0]),
			Kind:    parts[1],
			Path:    decodeB64(parts[2]),
			Size:    size,
			Mtime:   mtime,
			Archive: parts[5] == "1",
		})
	}
	return files, nil
}

// LogTail 取日志尾部；返回内容不含脚本的状态行。
func (r *Runner) LogTail(server sshc.Server, target string, lines int) (string, error) {
	if lines < 1 {
		lines = 200
	}
	if lines > 2000 {
		lines = 2000
	}
	// 第一次调用偶尔拿不到分隔线（连接刚建立时的老问题），重试一次就稳了；
	// 仍然拿不到时把内容原样返回，总比给用户一个空面板强。
	for attempt := 0; attempt < 2; attempt++ {
		text, marker, err := r.logTailOnce(server, target, lines)
		if err != nil {
			return "", err
		}
		if marker {
			return text, nil
		}
		if attempt == 1 {
			return text, nil
		}
	}
	return "", nil
}

// logTailOnce 跑一次 logs.sh tail，返回（内容, 是否拿到了分隔线）。
func (r *Runner) logTailOnce(server sshc.Server, target string, lines int) (string, bool, error) {
	env := map[string]string{
		"LOG_ACTION": "tail",
		"LOG_TARGET": target,
		"LOG_LINES":  strconv.Itoa(lines),
	}
	res, err := r.RunUploadedScript(server, "logs.sh", env, nil, 60*time.Second)
	if err != nil {
		return "", false, err
	}
	out := strings.ReplaceAll(StripANSI(res.Stdout), "\r\n", "\n")
	if res.Code != 0 {
		return "", false, fmt.Errorf("%s", errText(res, "读取日志失败"))
	}
	if idx := strings.Index(out, "---8<---"); idx >= 0 {
		body := out[idx+len("---8<---"):]
		return strings.TrimPrefix(body, "\n"), true, nil
	}
	// 没有分隔线：去掉脚本自己的状态行，剩下的当内容
	var kept []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "PANEL_") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.TrimLeft(strings.Join(kept, "\n"), "\n"), false, nil
}

// LogAction 清空 / 轮转 / 删除归档。
func (r *Runner) LogAction(server sshc.Server, action, target string, onData func(string)) (*sshc.Result, error) {
	env := map[string]string{"LOG_ACTION": action, "LOG_TARGET": target}
	res, err := r.RunUploadedScript(server, "logs.sh", env, onData, 3*time.Minute)
	if err != nil {
		return nil, err
	}
	if res != nil && res.Code != 0 {
		return res, fmt.Errorf("%s", errText(res, "日志操作失败"))
	}
	return res, nil
}

// ---------------------------------------------------------------- 定时任务

// CronEntry 是 crontab 里的一条任务。
type CronEntry struct {
	Line     int    `json:"line"`
	Enabled  bool   `json:"enabled"`
	Managed  bool   `json:"managed"` // 是否面板创建
	ID       string `json:"id"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
	Label    string `json:"label"`
}

// CronStatus 是定时任务页面的数据。
type CronStatus struct {
	Available bool        `json:"available"`
	Service   string      `json:"service"` // running / stopped / none
	Entries   []CronEntry `json:"entries"`
	Total     int         `json:"total"`
}

// CronParams 是一次定时任务操作。
type CronParams struct {
	Action   string
	ID       string
	Line     int
	Schedule string
	Command  string
	Label    string
	Enabled  bool
}

// Cron 读取 root crontab。
func (r *Runner) Cron(server sshc.Server) (*CronStatus, error) {
	res, err := r.RunUploadedScript(server, "cron.sh", map[string]string{"CRON_ACTION": "list"}, nil, 60*time.Second)
	if err != nil {
		return nil, err
	}
	out := StripANSI(res.Stdout)
	if res.Code != 0 {
		return nil, fmt.Errorf("%s", errText(res, "读取定时任务失败"))
	}
	return ParseCronStatus(out), nil
}

// CronAction 增 / 删 / 启停 / 立即执行；返回最新列表。
func (r *Runner) CronAction(server sshc.Server, p CronParams, onData func(string)) (*CronStatus, *sshc.Result, error) {
	env := map[string]string{"CRON_ACTION": p.Action}
	if p.ID != "" {
		env["CRON_ID"] = p.ID
	}
	if p.Line > 0 {
		env["CRON_LINE"] = strconv.Itoa(p.Line)
	}
	if p.Schedule != "" {
		env["CRON_SCHEDULE"] = p.Schedule
	}
	if p.Command != "" {
		env["CRON_COMMAND"] = p.Command
	}
	if p.Label != "" {
		env["CRON_LABEL"] = p.Label
	}
	if p.Enabled {
		env["CRON_ENABLED"] = "1"
	} else {
		env["CRON_ENABLED"] = "0"
	}

	timeout := 3 * time.Minute
	if p.Action == "run" {
		timeout = 4 * time.Minute
	}
	res, err := r.RunUploadedScript(server, "cron.sh", env, onData, timeout)
	if err != nil {
		return nil, nil, err
	}
	out := ""
	if res != nil {
		out = StripANSI(res.Stdout)
	}
	st := ParseCronStatus(out)
	if res.Code != 0 {
		return st, res, fmt.Errorf("%s", errText(res, "定时任务操作失败"))
	}
	return st, res, nil
}

// ParseCronStatus 解析 cron.sh 的 PANEL_ 输出。
func ParseCronStatus(out string) *CronStatus {
	st := &CronStatus{Entries: []CronEntry{}, Service: "none"}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "PANEL_") {
			continue
		}
		kv := strings.SplitN(strings.TrimPrefix(line, "PANEL_"), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := kv[0], kv[1]
		switch key {
		case "CRON_AVAIL":
			st.Available = val == "1"
		case "CRON_SVC":
			st.Service = val
		case "CRON_TOTAL":
			st.Total, _ = strconv.Atoi(val)
		case "CRON_ENTRY":
			parts := strings.Split(val, "|")
			if len(parts) < 7 {
				continue
			}
			n, _ := strconv.Atoi(parts[0])
			// 非面板创建的任务在 crontab 里没有标记注释，本来 id 是空串。
			// 空串在前端不好当 key（"" 与数字 id 混着用），这里统一成 line-<行号>：
			// 类型恒为字符串，删的时候按前缀还原成行号即可。
			id := parts[3]
			if id == "" {
				id = "line-" + parts[0]
			}
			st.Entries = append(st.Entries, CronEntry{
				Line:     n,
				Enabled:  parts[1] == "1",
				Managed:  parts[2] == "1",
				ID:       id,
				Schedule: decodeB64(parts[4]),
				Command:  decodeB64(parts[5]),
				Label:    decodeB64(parts[6]),
			})
		}
	}
	return st
}

// ---------------------------------------------------------------- 执行命令

// RunCommand 在远端以 root 执行一条命令，输出边跑边回调。
func (r *Runner) RunCommand(server sshc.Server, command, cwd string, timeout time.Duration, onData func(string)) (*sshc.Result, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("命令不能为空")
	}
	if timeout <= 0 || timeout > 10*time.Minute {
		timeout = 2 * time.Minute
	}

	var b strings.Builder
	b.WriteString("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n")
	b.WriteString("export LANG=C.UTF-8\n")
	if cwd != "" {
		b.WriteString("cd " + sshc.ShellQuote(cwd) + " || { printf '[err] 目录不存在：%s\\n' " + sshc.ShellQuote(cwd) + " >&2; exit 9; }\n")
	}
	b.WriteString(command)
	if !strings.HasSuffix(command, "\n") {
		b.WriteString("\n")
	}

	// base64 传输，避免命令里的引号被 shell 二次解释
	enc := base64.StdEncoding.EncodeToString([]byte(b.String()))
	runner := fmt.Sprintf("echo %s | base64 -d | bash", enc)
	if server.Username != "root" {
		runner = fmt.Sprintf("sudo -n bash -c %s", sshc.ShellQuote(runner))
	}
	return r.Pool.Exec(server, runner, &sshc.ExecOptions{
		Timeout: timeout,
		OnData: func(text string, _ bool) {
			if onData != nil {
				onData(text)
			}
		},
	})
}
