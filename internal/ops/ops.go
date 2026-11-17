// Package ops：运维操作——脚本上传执行、环境体检、初始化、建站、备份。
package ops

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"wpqu-panel/internal/sites"
	"wpqu-panel/internal/sshc"
	"wpqu-panel/internal/store"
)

// Runner 把脚本内容（嵌入 FS）+ 存储路径绑在一起。
type Runner struct {
	Pool    *sshc.Pool
	Scripts fs.FS // embed 的 scripts 目录
	Paths   store.Paths
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// StripANSI 去掉颜色控制符。
func StripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func (r *Runner) readScript(name string) (string, error) {
	raw, err := fs.ReadFile(r.Scripts, "scripts/"+name)
	if err != nil {
		return "", fmt.Errorf("找不到脚本 %s", name)
	}
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n"), nil
}

// RunUploadedScript 上传脚本到 /tmp 再以 root 执行；结束时清理。
func (r *Runner) RunUploadedScript(server sshc.Server, scriptName string, envVars map[string]string, onData func(string), timeout time.Duration) (*sshc.Result, error) {
	content, err := r.readScript(scriptName)
	if err != nil {
		return nil, err
	}
	return r.RunScriptText(server, scriptName, content, envVars, onData, timeout)
}

// RunScriptText 执行任意脚本文本。
// 插件包里的安装脚本不在 embed FS 里，但执行方式（上传 → 校验大小 → sudo bash → 清理）
// 与内置脚本保持完全一致，避免出现「插件脚本行为跟建站脚本不一样」的诡异问题。
func (r *Runner) RunScriptText(server sshc.Server, name, content string, envVars map[string]string, onData func(string), timeout time.Duration) (*sshc.Result, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "plugin.sh"
	}
	remote := fmt.Sprintf("/tmp/.wp-panel-%d-%s", time.Now().UnixMilli(), name)
	if err := r.Pool.UploadBuffer(server, remote, []byte(content)); err != nil {
		return nil, err
	}

	// 上传后校验大小，避免 SFTP 静默截断
	if st, err := r.Pool.StatPath(server, remote); err == nil {
		if st.Size != int64(len(content)) {
			_, _ = r.Pool.Exec(server, sshc.WithRoot(server, "rm -f "+sshc.ShellQuote(remote)), &sshc.ExecOptions{Timeout: 10 * time.Second})
			return nil, fmt.Errorf("脚本上传不完整（远端 %d 字节，本地 %d 字节）", st.Size, len(content))
		}
	}

	var parts []string
	keys := make([]string, 0, len(envVars))
	for k := range envVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := envVars[k]; v != "" {
			parts = append(parts, fmt.Sprintf("%s=%s", k, sshc.ShellQuote(v)))
		}
	}
	prefix := strings.Join(parts, " ")

	runner := fmt.Sprintf("env %s bash %s", prefix, remote)
	if server.Username != "root" {
		runner = fmt.Sprintf("sudo -n env %s bash %s", prefix, remote)
	}

	res, err := r.Pool.Exec(server, runner+" 2>&1", &sshc.ExecOptions{
		Timeout: timeout,
		OnData: func(text string, _ bool) {
			if onData != nil {
				onData(text)
			}
		},
	})
	// 无论成败都清理远端脚本
	go func() {
		_, _ = r.Pool.Exec(server, sshc.WithRoot(server, "rm -f "+sshc.ShellQuote(remote)), &sshc.ExecOptions{Timeout: 10 * time.Second})
	}()
	if err != nil {
		return nil, err
	}
	return res, nil
}

// TailRemoteLog 取远端最新运维日志尾部（失败诊断用）。
func (r *Runner) TailRemoteLog(server sshc.Server, pattern string) (string, error) {
	script := `
set +e
F=$(ls -1t /var/log/` + pattern + `*.log 2>/dev/null | head -1)
if [ -z "$F" ]; then echo "__NO_LOG__"; exit 0; fi
if [ -n "$(find "$F" -mmin +10 2>/dev/null)" ]; then echo "__STALE__$F"; exit 0; fi
echo "__LOG_FILE__$F"
tail -n 60 "$F" 2>/dev/null
`
	res, err := sites.RunScript(r.Pool, server, script, 30*time.Second)
	if err != nil {
		return "", err
	}
	out := StripANSI(res.Stdout)
	if strings.Contains(out, "__NO_LOG__") || strings.Contains(out, "__STALE__") {
		return "", nil
	}
	return strings.Replace(out, "__LOG_FILE__", "远端日志：", 1), nil
}

// ---------------------------------------------------------------- 体检

const doctorScript = `
export LC_ALL=C
` + `
` + `
DB_CLI=$(command -v mariadb || command -v mysql || true)
DB_DUMP=$(command -v mariadb-dump || command -v mysqldump || true)
DB_SVC=""
for __svc in mariadb mysql; do
  if systemctl is-active --quiet "$__svc" 2>/dev/null; then DB_SVC="$__svc"; break; fi
done
DB_VER_RAW=""
if [ -n "$DB_CLI" ]; then
  DB_VER_RAW=$("$DB_CLI" -N -B -e 'SELECT VERSION()' 2>/dev/null | head -1)
  [ -n "$DB_VER_RAW" ] || DB_VER_RAW=$("$DB_CLI" -V 2>/dev/null)
fi
DB_FLAVOR=none
case "$DB_VER_RAW" in
  *[Mm]ariaDB*) DB_FLAVOR=mariadb ;;
  ?*)           DB_FLAVOR=mysql ;;
esac
DB_VER=$(printf '%s' "$DB_VER_RAW" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | tail -1)
echo "USER=$(id -un 2>/dev/null)"
echo "UID=$(id -u 2>/dev/null)"
if [ "$(id -u 2>/dev/null)" = "0" ]; then
  echo "SUDO=root"
elif sudo -n true 2>/dev/null; then
  echo "SUDO=ok"
else
  echo "SUDO=need-password"
fi
echo "HOST=$(hostname 2>/dev/null)"
echo "OS=$(. /etc/os-release 2>/dev/null; echo ${PRETTY_NAME:-unknown})"
for c in nginx php mariadb mysql mariadb-dump mysqldump wp certbot curl tar gzip openssl runuser systemctl; do
  if command -v "$c" >/dev/null 2>&1; then echo "CMD_$c=1"; else echo "CMD_$c=0"; fi
done
echo "NGINX_ACTIVE=$(systemctl is-active nginx 2>/dev/null)"
echo "APACHE_ACTIVE=$(systemctl is-active apache2 2>/dev/null)"
echo "APACHE_VER=$(apache2 -v 2>/dev/null | sed -nE 's|.*Apache/([0-9.]+).*|\1|p' | head -1)"
echo "APACHETEST=$(apache2ctl configtest 2>&1 | tr '\n' ' ' | cut -c1-200)"
echo "APACHESITES=$(ls -1 /etc/apache2/sites-enabled 2>/dev/null | tr '\n' ',')"
echo "MARIADB_ACTIVE=$(systemctl is-active mariadb 2>/dev/null)"
echo "MYSQL_ACTIVE=$(systemctl is-active mysql 2>/dev/null)"
echo "FPM_ACTIVE=$(systemctl list-units --type=service --state=running --no-legend 'php*-fpm*' 2>/dev/null | awk '{print $1}' | tr '\n' ',')"
echo "SITES=$(ls -1 /etc/nginx/sites-enabled 2>/dev/null | tr '\n' ',')"
echo "VARWWW=$(ls -1 /var/www 2>/dev/null | tr '\n' ',')"
echo "PHPVERS=$(ls -1d /etc/php/*/fpm 2>/dev/null | awk -F/ '{print $(NF-1)}' | tr '\n' ',')"
WPBIN=$(command -v wp 2>/dev/null)
if [ -n "$WPBIN" ] && [ -s "$WPBIN" ] && php "$WPBIN" --info --allow-root >/dev/null 2>&1; then
  echo "WPCLI=ok"
  echo "WPCLIVER=$(php "$WPBIN" --version --allow-root 2>/dev/null | head -1)"
else
  echo "WPCLI=bad"
  echo "WPBINSIZE=$(stat -c %s "$WPBIN" 2>/dev/null || echo 0)"
fi
echo "MEMAVAIL=$(awk '/MemAvailable/{printf "%d", $2/1024}' /proc/meminfo)"
echo "DISKFREE=$(df -h / | awk 'NR==2{print $4}')"
echo "DBPING=$("$DB_CLI" -N -B -e 'SELECT 1' 2>&1 | tr '\n' ' ' | cut -c1-120)"
echo "DBFLAVOR=$DB_FLAVOR"
echo "DBVER=$DB_VER"
echo "DBSVC=$DB_SVC"
echo "NGINXTEST=$(nginx -t 2>&1 | tr '\n' ' ' | cut -c1-200)"
# 网络环境：装包/下载脚本按这个结果挑软件源，这里给出同样的判断（共用 12 小时缓存）
NR=$(sed -n 's/^REGION=//p' /var/lib/wpqu/net.env 2>/dev/null | head -1)
NRTS=$(sed -n 's/^TS=//p' /var/lib/wpqu/net.env 2>/dev/null | head -1)
case "$NRTS" in '' | *[!0-9]*) NRTS=0 ;; esac
if [ -z "$NR" ] || [ $(($(date +%s) - NRTS)) -ge 43200 ]; then
  NRD=$(mktemp -d 2>/dev/null)
  if [ -n "$NRD" ]; then
    ( curl -s -o /dev/null -m 2 -w '%{http_code} %{time_total}' https://mirrors.aliyun.com/ >"$NRD/a" 2>/dev/null ) &
    ( curl -s -o /dev/null -m 2 -w '%{http_code} %{time_total}' https://mirrors.cloud.tencent.com/ >"$NRD/b" 2>/dev/null ) &
    ( curl -s -o /dev/null -m 2 -w '%{http_code} %{time_total}' https://www.google.com/generate_204 >"$NRD/c" 2>/dev/null ) &
    ( curl -s -r 0-2047 -o /dev/null -m 2 -w '%{http_code} %{time_total}' https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar >"$NRD/d" 2>/dev/null ) &
    wait
    CN=0; CNS=0; GL=0; GLS=0
    for f in a b; do
      code=000; t=9
      read -r code t <"$NRD/$f" 2>/dev/null || true
      if [ "${code:-000}" != "000" ]; then
        CN=$((CN + 1))
        CNS=$(awk -v x="$CNS" -v y="${t:-9}" 'BEGIN{printf "%.3f", x+y}')
      fi
    done
    for f in c d; do
      code=000; t=9
      read -r code t <"$NRD/$f" 2>/dev/null || true
      if [ "${code:-000}" != "000" ]; then
        GL=$((GL + 1))
        GLS=$(awk -v x="$GLS" -v y="${t:-9}" 'BEGIN{printf "%.3f", x+y}')
      fi
    done
    rm -rf "$NRD"
    if [ "$CN" -gt 0 ] && [ "$GL" -eq 0 ]; then
      NR=cn
    elif [ "$GL" -gt 0 ] && [ "$CN" -eq 0 ]; then
      NR=global
    elif [ "$CN" -gt 0 ] && [ "$GL" -gt 0 ]; then
      NR=$(awk -v c="$CN" -v cs="$CNS" -v g="$GL" -v gs="$GLS" 'BEGIN{ print (gs/g < (cs/c)*0.8) ? "global" : "cn" }')
    else
      NR=unknown
    fi
    mkdir -p /var/lib/wpqu 2>/dev/null || true
    printf 'TS=%s\nREGION=%s\n' "$(date +%s)" "$NR" >/var/lib/wpqu/net.env 2>/dev/null || true
  fi
fi
[ -n "$NR" ] || NR=unknown
echo "NETREGION=$NR"
echo "LOGS=$(ls -1t /var/log/wp-*.log 2>/dev/null | head -5 | tr '\n' ',')"
echo "__END__"
`

// Check 是体检的一个检查项。
type Check struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	OK    bool   `json:"ok"`
	Value string `json:"value"`
	Hint  string `json:"hint"`
}

// DoctorResult 是 /doctor 的响应。
type DoctorResult struct {
	OK     bool              `json:"ok"`
	Raw    map[string]string `json:"raw"`
	Checks []Check           `json:"checks"`
	Host   string            `json:"host"`
	OS     string            `json:"os"`
	User   string            `json:"user"`
	// WebServer 是当前实际在跑的 Web 服务器：nginx / apache / none。
	// 站点的增删改脚本只写了 nginx 一套，apache 机器要在界面上提前劝住。
	WebServer string   `json:"webServer"`
	Logs      []string `json:"logs"`
}

var doctorKeyRe = regexp.MustCompile(`^(CMD_[a-z-]+|[A-Z_]+)$`)
var dbPingBadRe = regexp.MustCompile(`(?i)ERROR|denied|Can't connect`)

// Doctor 环境体检。
func (r *Runner) Doctor(server sshc.Server) (*DoctorResult, error) {
	res, err := sites.RunScript(r.Pool, server, doctorScript, 45*time.Second)
	if err != nil {
		return nil, err
	}
	kv := map[string]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		idx := strings.Index(line, "=")
		if idx > 0 {
			key := strings.TrimSpace(line[:idx])
			if doctorKeyRe.MatchString(key) {
				kv[key] = strings.TrimSpace(line[idx+1:])
			}
		}
	}

	isRoot := kv["UID"] == "0"
	sudoOk := isRoot || kv["SUDO"] == "ok"
	hasDb := kv["CMD_mariadb"] == "1" || kv["CMD_mysql"] == "1"
	dbOk := hasDb && !dbPingBadRe.MatchString(kv["DBPING"])
	nginxOk := kv["CMD_nginx"] == "1" && kv["NGINX_ACTIVE"] == "active"
	apacheOk := kv["APACHE_ACTIVE"] == "active"
	webServer := "none"
	if nginxOk {
		webServer = "nginx"
	} else if apacheOk {
		webServer = "apache"
	}
	phpOk := kv["CMD_php"] == "1" || len(kv["PHPVERS"]) > 0
	fpmActive := strings.TrimSuffix(kv["FPM_ACTIVE"], ",")
	fpmOk := fpmActive != ""
	nginxTestOk := strings.Contains(kv["NGINXTEST"], "successful")

	privValue := func() string {
		if isRoot {
			return "root"
		}
		if kv["SUDO"] == "ok" {
			return fmt.Sprintf("sudo 免密（%s）", kv["USER"])
		}
		if kv["USER"] == "" {
			kv["USER"] = "?"
		}
		return fmt.Sprintf("%s，sudo 需要密码", kv["USER"])
	}()
	privHint := ""
	if !sudoOk {
		privHint = "新增/变更站点必须以 root 执行，或给该账号配置免密 sudo"
	}

	webLabel := "Nginx"
	nginxValue := func() string {
		if apacheOk && !nginxOk {
			if kv["APACHE_VER"] != "" {
				return "运行中（Apache " + kv["APACHE_VER"] + "）"
			}
			return "运行中（Apache）"
		}
		switch {
		case nginxOk:
			return "运行中"
		case kv["CMD_nginx"] == "1":
			return fmt.Sprintf("已安装但状态为 %s", kv["NGINX_ACTIVE"])
		default:
			return "未安装"
		}
	}()
	nginxHint := ""
	if apacheOk && !nginxOk {
		webLabel = "Web 服务器"
		nginxHint = "这台机器用的是 Apache（mpm_event + php-fpm），站点操作由面板按 VirtualHost 维护"
	} else if !nginxOk {
		nginxHint = "先执行 install-wordpress.sh，或 systemctl start nginx / apache2"
	}

	phpVers := strings.TrimSuffix(kv["PHPVERS"], ",")
	cli := "无"
	if kv["CMD_php"] == "1" {
		cli = "有"
	}
	fpmTxt := "未运行"
	if fpmOk {
		fpmTxt = fpmActive
	}
	phpValue := fmt.Sprintf("CLI %s／FPM %s", cli, fpmTxt)
	if phpVers != "" {
		phpValue += "／版本 " + phpVers
	}
	phpHint := ""
	if !(phpOk && fpmOk) {
		phpHint = "缺少 php 命令或 FPM 未运行，脚本无法确定 socket 和版本"
	}

	dbValue := func() string {
		if !hasDb {
			return "未安装 mariadb/mysql 客户端"
		}
		if dbOk {
			flavor := "数据库"
			switch kv["DBFLAVOR"] {
			case "mysql":
				flavor = "MySQL"
			case "mariadb":
				flavor = "MariaDB"
			}
			if kv["DBVER"] != "" {
				return fmt.Sprintf("%s %s · root 免密可连", flavor, kv["DBVER"])
			}
			return flavor + " · root 免密可连"
		}
		ping := kv["DBPING"]
		if len(ping) > 80 {
			ping = ping[:80]
		}
		return "连接异常：" + ping
	}()
	dbHint := ""
	if !dbOk {
		dbHint = "新建站点需要创建数据库；确认数据库服务已启动且 root 可用"
	}

	wpValue := func() string {
		if kv["WPCLI"] == "ok" {
			if kv["WPCLIVER"] != "" {
				return kv["WPCLIVER"]
			}
			return "可用"
		}
		if kv["CMD_wp"] == "1" {
			return fmt.Sprintf("文件存在但无法运行（%s 字节，0 字节说明下载失败）", kv["WPBINSIZE"])
		}
		return "未安装"
	}()
	wpHint := ""
	if kv["WPCLI"] != "ok" {
		wpHint = "站点安装与域名变更都依赖它；重新执行 install-wordpress.sh 可从镜像自动补装"
	}

	sitesPath := "/etc/nginx/sites-enabled"
	sitesRaw := kv["SITES"]
	if apacheOk && !nginxOk {
		sitesPath = "/etc/apache2/sites-enabled"
		sitesRaw = kv["APACHESITES"]
		// Debian 自带的默认站点不是用户站点，混在列表里会让人以为面板漏了什么
		filtered := make([]string, 0, 4)
		for _, n := range strings.Split(sitesRaw, ",") {
			n = strings.TrimSuffix(strings.TrimSpace(n), ".conf")
			if n == "" || n == "000-default" {
				continue
			}
			filtered = append(filtered, n)
		}
		sitesRaw = strings.Join(filtered, ",")
	}
	sitesList := strings.TrimSuffix(sitesRaw, ",")
	sitesList = strings.TrimSuffix(sitesList, ".conf")
	sitesOk := sitesList != ""
	sitesHint := ""
	if !sitesOk {
		sitesHint = sitesPath + " 下没有站点配置"
	}

	nginxTestValue := kv["NGINXTEST"]
	if apacheOk && !nginxOk {
		nginxTestValue = kv["APACHETEST"]
		nginxTestOk = strings.Contains(nginxTestValue, "Syntax OK")
	}
	if nginxTestValue == "" {
		nginxTestValue = "无输出"
	}
	if len(nginxTestValue) > 120 {
		nginxTestValue = nginxTestValue[:120]
	}
	nginxTestHint := ""
	if !nginxTestOk {
		nginxTestHint = "配置校验未通过，新增/变更站点都会在这一步失败"
	}

	certbotOk := kv["CMD_certbot"] == "1"
	certbotHint := ""
	if !certbotOk {
		certbotHint = "缺证书时 HTTPS 会被跳过，站点仍可用 HTTP"
	}

	// 网络环境决定装包与下载走哪个源：国内走阿里云/校园镜像，海外走官方源
	netRegion := kv["NETREGION"]
	netValue := "未识别"
	netHint := ""
	switch netRegion {
	case "cn":
		netValue = "国内 · 使用国内镜像"
		netHint = "apt / pip / npm / GitHub 都走就近镜像；装包和建站会明显更快"
	case "global":
		netValue = "海外 · 使用官方源"
		netHint = "直连官方源最快，脚本不会套国内镜像"
	default:
		netHint = "两组探测地址都不通，脚本会按官方源尝试；可在 /etc/wpqu/net.conf 里写 REGION=cn 或 global 强制指定"
	}

	memAvail := atoiSafe(kv["MEMAVAIL"])
	resourceOk := memAvail > 250
	resourceHint := ""
	if !resourceOk {
		resourceHint = "可用内存偏低，再起一个站容易 OOM"
	}
	diskFree := kv["DISKFREE"]
	if diskFree == "" {
		diskFree = "?"
	}
	resourceValue := fmt.Sprintf("可用内存 %d MB／根分区剩余 %s", memAvail, diskFree)

	checks := []Check{
		{Key: "privilege", Label: "执行权限", OK: sudoOk, Value: privValue, Hint: privHint},
		{Key: "nginx", Label: webLabel, OK: nginxOk || apacheOk, Value: nginxValue, Hint: nginxHint},
		{Key: "php", Label: "PHP-FPM", OK: phpOk && fpmOk, Value: phpValue, Hint: phpHint},
		{Key: "db", Label: "数据库", OK: dbOk, Value: dbValue, Hint: dbHint},
		{Key: "wpcli", Label: "WP-CLI", OK: kv["WPCLI"] == "ok", Value: wpValue, Hint: wpHint},
		{Key: "sites", Label: "现有站点", OK: sitesOk, Value: func() string {
			if sitesOk {
				return sitesList
			}
			return "sites-enabled 为空"
		}(), Hint: sitesHint},
		{Key: "nginxconf", Label: func() string {
			if apacheOk && !nginxOk {
				return "Apache 配置校验"
			}
			return "Nginx 配置校验"
		}(), OK: nginxTestOk, Value: nginxTestValue, Hint: nginxTestHint},
		{Key: "certbot", Label: "Certbot", OK: certbotOk, Value: func() string {
			if certbotOk {
				return "已安装"
			}
			return "未安装"
		}(), Hint: certbotHint},
		{Key: "net", Label: "网络环境", OK: netRegion != "unknown", Value: netValue, Hint: netHint},
		{Key: "resource", Label: "资源", OK: resourceOk, Value: resourceValue, Hint: resourceHint},
	}

	allOk := true
	for _, c := range checks {
		if !c.OK {
			allOk = false
		}
	}
	return &DoctorResult{
		OK: allOk, Raw: kv, Checks: checks,
		Host: kv["HOST"], OS: kv["OS"], User: kv["USER"], WebServer: webServer,
		Logs: strings.FieldsFunc(kv["LOGS"], func(r rune) bool { return r == ',' }),
	}, nil
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// DoctorError 带体检结果的错误（assertReady 用）。
type DoctorError struct {
	Msg    string
	Doctor *DoctorResult
}

func (e *DoctorError) Error() string { return e.Msg }

// AssertReady 跑运维脚本前的硬性前置校验。
func (r *Runner) AssertReady(server sshc.Server, kind string) (*DoctorResult, error) {
	d, err := r.Doctor(server)
	if err != nil {
		return nil, err
	}
	byKey := map[string]Check{}
	for _, c := range d.Checks {
		byKey[c.Key] = c
	}
	var problems []string
	if !byKey["privilege"].OK {
		problems = append(problems, byKey["privilege"].Hint)
	}
	if !byKey["nginx"].OK {
		problems = append(problems, byKey["nginx"].Hint)
	}
	if !byKey["nginxconf"].OK {
		problems = append(problems, fmt.Sprintf("nginx -t 未通过：%s", byKey["nginxconf"].Value))
	}
	if kind != "static" && !byKey["db"].OK {
		problems = append(problems, byKey["db"].Hint)
	}
	if kind == "domain" && !byKey["wpcli"].OK {
		problems = append(problems, byKey["wpcli"].Hint)
	}
	if len(problems) > 0 {
		return d, &DoctorError{Msg: "环境未就绪，已中止（未做任何改动）：\n- " + strings.Join(problems, "\n- "), Doctor: d}
	}
	return d, nil
}

// ---------------------------------------------------------------- 运维操作参数

// BootstrapParams 初始化环境参数。
// WebServer / PHPVersion 是「自定义选装」才传的：留空即默认组合
// （Nginx + 发行版自带 PHP + MariaDB）。
type BootstrapParams struct {
	Domain     string
	Title      string
	AdminUser  string
	AdminPass  string
	AdminEmail string
	WebServer  string
	PHPVersion string
	DBEngine   string
	SSL        bool
	UFW        bool
	Redis      bool
}

func (r *Runner) BootstrapServer(server sshc.Server, p BootstrapParams, onData func(string)) (*sshc.Result, error) {
	engine := "mariadb"
	switch p.DBEngine {
	case "mysql84", "mysql8":
		engine = "mysql84"
	case "mysql57":
		engine = "mysql57"
	}
	web := "nginx"
	if p.WebServer == "apache" {
		web = "apache"
	}
	ssl, ufw, redis := "0", "0", "0"
	if p.SSL {
		ssl = "1"
	}
	if p.UFW {
		ufw = "1"
	}
	if p.Redis {
		redis = "1"
	}
	env := map[string]string{
		"DOMAIN": p.Domain, "SITE_TITLE": p.Title,
		"ADMIN_USER": p.AdminUser, "ADMIN_PASS": p.AdminPass, "ADMIN_EMAIL": p.AdminEmail,
		"WEB_SERVER": web, "DB_ENGINE": engine,
		"ENABLE_SSL": ssl, "ENABLE_UFW": ufw, "INSTALL_REDIS": redis,
		"DEBIAN_FRONTEND": "noninteractive",
	}
	// 不传 PHP_VERSION 时脚本走发行版默认版本，别把空值塞进环境变量
	if p.PHPVersion != "" {
		env["PHP_VERSION"] = p.PHPVersion
	}
	return r.RunUploadedScript(server, "install-wordpress.sh", env, onData, 25*time.Minute)
}

// AddSiteParams 建站参数。
type AddSiteParams struct {
	Domain       string
	Type         string
	Title        string
	AdminUser    string
	AdminPass    string
	AdminEmail   string
	Locale       string
	MaxChildren  string
	SeparatePool string
	SSL          bool
}

func (r *Runner) AddSite(server sshc.Server, p AddSiteParams, onData func(string)) (*sshc.Result, error) {
	t := p.Type
	if t != "wordpress" && t != "custom" && t != "static" {
		t = "wordpress"
	}
	if _, err := r.AssertReady(server, map[bool]string{true: "static", false: "add"}[t == "static"]); err != nil {
		return nil, err
	}
	ssl := "0"
	if p.SSL {
		ssl = "1"
	}
	return r.RunUploadedScript(server, "add-site.sh", map[string]string{
		"DOMAIN": p.Domain, "SITE_TYPE": t, "SITE_TITLE": p.Title,
		"ADMIN_USER": p.AdminUser, "ADMIN_PASS": p.AdminPass, "ADMIN_EMAIL": p.AdminEmail,
		"WP_LOCALE": p.Locale, "PHP_MAX_CHILDREN": p.MaxChildren, "SEPARATE_POOL": p.SeparatePool,
		"ENABLE_SSL": ssl, "DEBIAN_FRONTEND": "noninteractive",
	}, onData, 15*time.Minute)
}

// ConvertParams 类型转换参数。
type ConvertParams struct {
	Domain       string
	TargetType   string
	Title        string
	AdminUser    string
	AdminPass    string
	AdminEmail   string
	Locale       string
	MaxChildren  string
	SeparatePool string
	Force        bool
	DryRun       bool
}

func (r *Runner) ConvertSiteType(server sshc.Server, p ConvertParams, onData func(string)) (*sshc.Result, error) {
	t := p.TargetType
	if t != "wordpress" && t != "custom" && t != "static" {
		return nil, fmt.Errorf("目标类型不合法")
	}
	if _, err := r.AssertReady(server, map[bool]string{true: "static", false: "convert"}[t == "static"]); err != nil {
		return nil, err
	}
	ssl := "0"
	if p.Force {
		_ = ssl
	}
	force, dry := "0", "0"
	if p.Force {
		force = "1"
	}
	if p.DryRun {
		dry = "1"
	}
	return r.RunUploadedScript(server, "site-convert.sh", map[string]string{
		"DOMAIN": p.Domain, "TARGET_TYPE": t, "SITE_TITLE": p.Title,
		"ADMIN_USER": p.AdminUser, "ADMIN_PASS": p.AdminPass, "ADMIN_EMAIL": p.AdminEmail,
		"WP_LOCALE": p.Locale, "PHP_MAX_CHILDREN": p.MaxChildren, "SEPARATE_POOL": p.SeparatePool,
		"FORCE": force, "DRY_RUN": dry, "DEBIAN_FRONTEND": "noninteractive",
	}, onData, 15*time.Minute)
}

// DomainParams 改域名参数。
type DomainParams struct {
	OldDomain    string
	NewDomain    string
	KeepRedirect bool
	MoveDir      bool
	SSL          bool
	DryRun       bool
}

func (r *Runner) ChangeDomain(server sshc.Server, p DomainParams, onData func(string)) (*sshc.Result, error) {
	if _, err := r.AssertReady(server, "domain"); err != nil {
		return nil, err
	}
	keep, move, ssl, dry := "0", "0", "0", "0"
	if p.KeepRedirect {
		keep = "1"
	}
	if p.MoveDir {
		move = "1"
	}
	if p.SSL {
		ssl = "1"
	}
	if p.DryRun {
		dry = "1"
	}
	return r.RunUploadedScript(server, "change-domain.sh", map[string]string{
		"OLD_DOMAIN": p.OldDomain, "NEW_DOMAIN": p.NewDomain,
		"KEEP_REDIRECT": keep, "MOVE_DIR": move, "ENABLE_SSL": ssl, "DRY_RUN": dry,
		"DEBIAN_FRONTEND": "noninteractive",
	}, onData, 15*time.Minute)
}

// DeleteSiteParams 是删除整站的入参。
type DeleteSiteParams struct {
	Domain      string
	DBName      string
	DeleteFiles bool
	DeleteDB    bool
	DeleteCert  bool
}

// DeleteSite 调远端脚本删站：nginx 配置、站点片段、目录、数据库、证书。
// 脚本里会先用 server_name 交叉校验，路径只允许 /var/www/<域名>。
func (r *Runner) DeleteSite(server sshc.Server, p DeleteSiteParams, onData func(string)) (*sshc.Result, error) {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return r.RunUploadedScript(server, "delete-site.sh", map[string]string{
		"DOMAIN":          p.Domain,
		"DB_NAME":         p.DBName,
		"DELETE_FILES":    b(p.DeleteFiles),
		"DELETE_DB":       b(p.DeleteDB),
		"DELETE_CERT":     b(p.DeleteCert),
		"DEBIAN_FRONTEND": "noninteractive",
	}, onData, 10*time.Minute)
}

// IssueCert 申请证书。
func (r *Runner) IssueCert(server sshc.Server, domain, email string, onData func(string)) (*sshc.Result, error) {
	if email == "" {
		email = "admin@" + domain
	}
	cmd := fmt.Sprintf(
		`command -v certbot >/dev/null 2>&1 || { echo "certbot 未安装，正在安装..."; apt-get install -y certbot python3-certbot-nginx; } && certbot --nginx -d %s --non-interactive --agree-tos --redirect --email %s 2>&1`,
		sshc.ShellQuote(domain), sshc.ShellQuote(email),
	)
	return r.Pool.Exec(server, sshc.WithRoot(server, cmd), &sshc.ExecOptions{
		Timeout: 10 * time.Minute,
		OnData: func(text string, _ bool) {
			if onData != nil {
				onData(text)
			}
		},
	})
}

// RenewCerts 续期全部证书。
func (r *Runner) RenewCerts(server sshc.Server, onData func(string)) (*sshc.Result, error) {
	return r.Pool.Exec(server, sshc.WithRoot(server, "certbot renew --nginx --non-interactive 2>&1"), &sshc.ExecOptions{
		Timeout: 10 * time.Minute,
		OnData: func(text string, _ bool) {
			if onData != nil {
				onData(text)
			}
		},
	})
}

// ---------------------------------------------------------------- 备份

// BackupFile 是下载到本地的一个备份文件。
type BackupFile struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Domain   string `json:"domain"`
	ServerID string `json:"serverId"`
}

// BackupSite 打包并下载站点（数据库 + 文件）。
func (r *Runner) BackupSite(server sshc.Server, domain string, includeDb, includeFiles bool, onData func(string)) ([]BackupFile, error) {
	stamp := time.Now().UTC().Format("20060102150405")
	remoteDir := fmt.Sprintf("/tmp/wp-panel-backup-%s", stamp)

	onData(fmt.Sprintf("[1/4] 正在远端打包 %s ...\n", domain))

	dbFlag, filesFlag := "0", "0"
	if includeDb {
		dbFlag = "1"
	}
	if includeFiles {
		filesFlag = "1"
	}

	script := `
set -e
DB_CLI=$(command -v mariadb || command -v mysql || true)
DB_DUMP=$(command -v mariadb-dump || command -v mysqldump || true)
ROOT=` + sshc.ShellQuote("/var/www/"+domain) + `
OUT=` + sshc.ShellQuote(remoteDir) + `
rm -rf "$OUT"
mkdir -p "$OUT"
echo "SIZE_BEFORE=$(du -sh "$ROOT" 2>/dev/null | cut -f1)"
if [ "` + dbFlag + `" = "1" ] && [ -f "$ROOT/wp-config.php" ]; then
  DB=$(awk -F"'" '/DB_NAME/ {print $4; exit}' "$ROOT/wp-config.php")
  if [ -n "$DB" ]; then
    echo "DUMPING_DB=$DB"
    [ -n "$DB_DUMP" ] || { echo "服务器上找不到 mariadb-dump / mysqldump" >&2; exit 127; }
    "$DB_DUMP" --single-transaction --quick --default-character-set=utf8mb4 "$DB" | gzip > "$OUT/db.sql.gz"
  fi
fi
if [ "` + filesFlag + `" = "1" ]; then
  echo "PACKING_FILES"
  tar czf "$OUT/files.tar.gz" -C "$ROOT" --exclude=wp-content/cache --exclude=wp-content/upgrade --exclude=wp-content/ai1wm-backups . 2>/dev/null
fi
ls -l "$OUT"
`
	res, err := sites.RunScript(r.Pool, server, script, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		switch {
		case strings.HasPrefix(line, "SIZE_BEFORE="):
			onData(fmt.Sprintf("[1/4] 站点体积：%s\n", strings.TrimPrefix(line, "SIZE_BEFORE=")))
		case strings.HasPrefix(line, "DUMPING_DB="):
			onData(fmt.Sprintf("[2/4] 导出数据库 %s\n", strings.TrimPrefix(line, "DUMPING_DB=")))
		case line == "PACKING_FILES":
			onData("[3/4] 打包站点文件 ...\n")
		}
	}
	if res.Code != 0 {
		tail := res.Stderr + res.Stdout
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return nil, fmt.Errorf("远端打包失败：%s", strings.TrimSpace(tail))
	}

	listRes, err := sites.RunScript(r.Pool, server, fmt.Sprintf("ls -1 %s 2>/dev/null", sshc.ShellQuote(remoteDir)), 15*time.Second)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(listRes.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("远端没有生成备份文件，检查站点目录是否存在")
	}

	localDir := filepath.Join(r.Paths.BackupDir, server.ID)
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		return nil, err
	}

	var saved []BackupFile
	for _, file := range files {
		remotePath := remoteDir + "/" + file
		localName := fmt.Sprintf("%s-%s-%s", domain, stamp, file)
		localPath := filepath.Join(localDir, localName)

		var size int64
		if st, err := r.Pool.StatPath(server, remotePath); err == nil {
			size = st.Size
		}
		onData(fmt.Sprintf("[4/4] 下载 %s（%s）...\n", file, FormatSize(size)))
		if err := r.Pool.DownloadToFile(server, remotePath, localPath, func(received int64) {
			if size > 0 && received%(1024*1024) < 65536 {
				pct := received * 100 / size
				if pct > 100 {
					pct = 100
				}
				onData(fmt.Sprintf("  %d%%\n", pct))
			}
		}); err != nil {
			return nil, err
		}
		st, err := os.Stat(localPath)
		if err != nil {
			return nil, err
		}
		saved = append(saved, BackupFile{Name: localName, Path: localPath, Size: st.Size(), Domain: domain, ServerID: server.ID})
		onData(fmt.Sprintf("  已保存：%s\n", localName))
	}

	_, _ = sites.RunScript(r.Pool, server, fmt.Sprintf("rm -rf %s", sshc.ShellQuote(remoteDir)), 20*time.Second)
	onData(fmt.Sprintf("\n备份完成，共 %d 个文件\n", len(saved)))
	return saved, nil
}

// ListBackups 列出某台服务器的本地备份。
func (r *Runner) ListBackups(serverID string) ([]map[string]any, error) {
	dir := filepath.Join(r.Paths.BackupDir, serverID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []map[string]any{}, nil
	}
	var out []map[string]any
	for _, e := range entries {
		st, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"name": e.Name(), "size": st.Size(), "mtime": st.ModTime().UnixMilli(),
		})
	}
	// 新的在前
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j]["mtime"].(int64) > out[i]["mtime"].(int64) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// FormatSize 字节数转可读文本。
func FormatSize(bytes int64) string {
	if bytes == 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB"}
	var n = float64(bytes)
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if n >= 10 || i == 0 {
		return fmt.Sprintf("%.0f %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

// ---------------------------------------------------------------- 失败诊断

var logPatterns = map[string]string{
	"site.add":         "wp-addsite-",
	"site.type":        "wp-site-convert-",
	"site.domain":      "wp-domain-change-",
	"site.delete":      "wp-delsite-",
	"server.bootstrap": "wp-deploy-",
}

// ReportFailure 失败时把远端日志尾部灌进输出。
func (r *Runner) ReportFailure(server sshc.Server, push func(string), seen, logType string) {
	push("\n──────── 失败诊断 ────────\n")
	pattern := "wp-"
	if p, ok := logPatterns[logType]; ok {
		pattern = p
	}
	tail, err := r.TailRemoteLog(server, pattern)
	if err != nil {
		push(fmt.Sprintf("读取远端日志失败：%v\n", err))
		return
	}
	if tail == "" {
		push("远端没有本次运行的日志：脚本很可能在启动阶段就退出了（换行符 CRLF、解释器或权限问题）。\n")
		return
	}
	seenSet := map[string]bool{}
	for _, l := range strings.Split(seen, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			seenSet[t] = true
		}
	}
	lines := strings.Split(tail, "\n")
	head := ""
	if len(lines) > 0 {
		head = lines[0]
		lines = lines[1:]
	}
	push(head + "\n")
	var extra []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t != "" && !seenSet[t] {
			extra = append(extra, l)
		}
	}
	if len(extra) == 0 {
		push("（日志与上方输出一致，没有额外信息）\n")
		return
	}
	push(strings.TrimRight(strings.Join(extra, "\n"), " \t\n") + "\n")
}

// ReportDoctor 把体检红项灌进输出。
func ReportDoctor(push func(string), d *DoctorResult) {
	var bad []Check
	for _, c := range d.Checks {
		if !c.OK {
			bad = append(bad, c)
		}
	}
	if len(bad) == 0 {
		return
	}
	push("\n──────── 环境体检发现的问题 ────────\n")
	for _, c := range bad {
		push(fmt.Sprintf("✗ %s：%s\n", c.Label, c.Value))
		if c.Hint != "" {
			push(fmt.Sprintf("  → %s\n", c.Hint))
		}
	}
}

// JSONMarshal 供路由层复用。
func JSONMarshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
