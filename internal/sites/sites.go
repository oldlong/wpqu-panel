// Package sites：远端脚本组装与解析（概览、站点列表、站点详情）。
// 脚本内容与早期实现逐行一致——远端行为不变，只换执行引擎。
package sites

import (
	"encoding/base64"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"wpqu-panel/internal/sshc"
)

// DBPrelude 与早期实现的 DB_PRELUDE 完全一致。
const DBPrelude = `
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
`

const overviewScript = DBPrelude + `
echo "MEM=$(free -m | awk '/Mem:/{print $2","$3","$7}')"
echo "SWAP=$(free -m | awk '/Swap:/{print $2","$3}')"
echo "DISK=$(df -h / | awk 'NR==2{print $2","$3","$5}')"
echo "LOAD=$(cut -d' ' -f1-3 /proc/loadavg)"
echo "CPU=$(nproc)"
echo "UPTIME=$(uptime -p 2>/dev/null | sed 's/^up //')"
echo "PHP=$(php -r 'echo PHP_VERSION;' 2>/dev/null)"
echo "NGINX=$(nginx -v 2>&1 | sed 's|.*/||')"
echo "APACHE=$(apache2 -v 2>/dev/null | sed -nE 's|.*Apache/([0-9.]+).*|\1|p' | sed -n 1p)"
if systemctl is-active --quiet nginx 2>/dev/null; then
  WEBSERVER=nginx
elif systemctl is-active --quiet apache2 2>/dev/null; then
  WEBSERVER=apache
elif command -v nginx >/dev/null 2>&1; then
  WEBSERVER=nginx
elif command -v apache2ctl >/dev/null 2>&1; then
  WEBSERVER=apache
else
  WEBSERVER=none
fi
echo "WEBSERVER=$WEBSERVER"
if [ "$WEBSERVER" = apache ]; then
  echo "SITECOUNT=$(ls -1 /etc/apache2/sites-enabled/*.conf 2>/dev/null | wc -l)"
else
  echo "SITECOUNT=$(ls -1 /etc/nginx/sites-enabled 2>/dev/null | wc -l)"
fi
echo "WPCOUNT=$(ls -d /var/www/*/wp-config.php 2>/dev/null | wc -l)"
echo "CERTBOT=$(command -v certbot >/dev/null 2>&1 && echo 1 || echo 0)"
echo "WPSUDO=$(id -u)"
# 概览页的「数据库」两格读的是下面这两个键：前端要版本号，后端要品类名
echo "MYSQL=${DB_VER}"
echo "DBFLAVOR=${DB_FLAVOR}"
`

const sitesScript = `
echo "###SITES"
WEB=nginx
if systemctl is-active --quiet apache2 2>/dev/null && ! systemctl is-active --quiet nginx 2>/dev/null; then
  WEB=apache
fi
if [ "$WEB" = apache ]; then
  SITE_FILES=$(ls -1 /etc/apache2/sites-enabled/*.conf 2>/dev/null)
else
  SITE_FILES=$(ls -1 /etc/nginx/sites-enabled/* 2>/dev/null)
fi
for f in $SITE_FILES; do
  [ -e "$f" ] || continue
  case "$(basename "$f")" in 000-default.conf|default|default.conf) continue ;; esac
  if [ "$WEB" = apache ]; then
    names=$(awk '/^[ \t]*ServerName[ \t]/ { sub(/^[ \t]*ServerName[ \t]+/, ""); sub(/#.*/, ""); print } /^[ \t]*ServerAlias[ \t]/ { sub(/^[ \t]*ServerAlias[ \t]+/, ""); sub(/#.*/, ""); print }' "$f" 2>/dev/null | tr -d '"' | tr '\n' ' ' | sed 's/ *$//')
    rootdir=$(awk '/^[ \t]*DocumentRoot[ \t]/ { sub(/^[ \t]*DocumentRoot[ \t]+/, ""); sub(/#.*/, ""); print; exit }' "$f" 2>/dev/null | tr -d '"')
    ssl=0
    grep -q "SSLCertificateFile" "$f" 2>/dev/null && ssl=1
  else
    names=$(awk '/server_name/ {for (i=2; i<=NF; i++) { gsub(/;/, "", $i); if ($i != ";" && $i != "") printf "%s ", $i } }' "$f" 2>/dev/null | sed 's/ *$//')
    rootdir=$(awk '$1 == "root" { gsub(/;/, "", $2); print $2; exit }' "$f" 2>/dev/null)
    ssl=0
    grep -q "ssl_certificate" "$f" 2>/dev/null && ssl=1
  fi
  # 换域名后旧域名只留一个 301 跳转，没有 web 根目录；列出来只会让人困惑
  [ -n "$rootdir" ] || continue
  wp=0
  db=""
  if [ -n "$rootdir" ] && [ -f "$rootdir/wp-config.php" ]; then
    wp=1
    db=$(awk -F"'" '/DB_NAME/ {print $4; exit}' "$rootdir/wp-config.php" 2>/dev/null)
  fi
  # 站点能力类型：优先读面板写的标记（新位置与旧的 Nginx 专用位置），
  # 其次按文件特征推断（兼容早期建的站）
  primary=$(printf '%s' "$names" | awk '{print $1}')
  stype=""
  if [ -n "$primary" ]; then
    for tf in "/etc/wp-panel/site-types/$primary" "/etc/nginx/site-types/$primary"; do
      [ -f "$tf" ] || continue
      stype=$(head -1 "$tf" 2>/dev/null | tr -d '[:space:]')
    done
  fi
  case "$stype" in
    wordpress|custom|static) ;;
    *)
      if [ -n "$rootdir" ] && [ -f "$rootdir/wp-settings.php" ] && [ -f "$rootdir/wp-config.php" ]; then
        stype=wordpress
      elif grep -qE 'fastcgi_pass|proxy:unix:' "$f" 2>/dev/null; then
        stype=custom
      else
        stype=static
      fi
      ;;
  esac
  # 自助网站没有 wp-config.php，库名只写在建站生成的信息文件里
  if [ -z "$db" ] && [ "$stype" = "custom" ] && [ -n "$primary" ] && [ -f "/root/wp-$primary-info.txt" ]; then
    db=$(awk -F'：' '/^数据库名/ {gsub(/[ \t]/, "", $2); print $2; exit}' "/root/wp-$primary-info.txt" 2>/dev/null)
  fi
  echo "SITE|$f|$names|$rootdir|$ssl|$wp|$db|$stype"
done
echo "###CERTS"
if command -v certbot >/dev/null 2>&1; then
  certbot certificates 2>/dev/null | awk '
    /Certificate Name:/ { name = $3 }
    /Domains:/ { d = $0; sub(/.*Domains: /, "", d); sub(/ .*/, "", d) }
    /Expiry Date:/ { print name "|" d "|" $3 }
  '
fi
echo "###END"
`

// RunScript 把脚本 base64 后交给远端 bash（避免多层引号转义出错）。
func RunScript(pool *sshc.Pool, server sshc.Server, script string, timeout time.Duration) (*sshc.Result, error) {
	b64 := base64.StdEncoding.EncodeToString([]byte(script))
	cmd := fmt.Sprintf("echo %s | base64 -d | bash", b64)
	return pool.Exec(server, sshc.WithRoot(server, cmd), &sshc.ExecOptions{Timeout: timeout})
}

// SiteEntry 是站点列表里的一项。
type SiteEntry struct {
	File          string   `json:"file"`
	Domains       []string `json:"domains"`
	PrimaryDomain string   `json:"primaryDomain"`
	Root          string   `json:"root"`
	SSL           bool     `json:"ssl"`
	IsWordPress   bool     `json:"isWordPress"`
	DBName        string   `json:"dbName"`
	Type          string   `json:"type"`
	CertExpiry    *string  `json:"certExpiry"`
	CertDaysLeft  *int     `json:"certDaysLeft"`
}

// Snapshot 是 /overview 的响应。
type Snapshot struct {
	Overview  map[string]any `json:"overview"`
	Sites     []SiteEntry    `json:"sites"`
	FetchedAt string         `json:"fetchedAt"`
}

// GetServerSnapshot 并发拉概览 + 站点列表。
func GetServerSnapshot(pool *sshc.Pool, server sshc.Server) (*Snapshot, error) {
	type res struct {
		out map[string]string
		err error
	}
	ovCh := make(chan res, 1)
	sitesCh := make(chan res, 1)

	go func() {
		r, err := RunScript(pool, server, overviewScript, 30*time.Second)
		if err != nil {
			ovCh <- res{err: err}
			return
		}
		if r.Code != 0 {
			msg := strings.TrimSpace(r.Stderr)
			if msg == "" {
				msg = "读取服务器信息失败"
			}
			ovCh <- res{err: fmt.Errorf("%s", msg)}
			return
		}
		ovCh <- res{out: parseKV(r.Stdout)}
	}()
	go func() {
		r, err := RunScript(pool, server, sitesScript, 45*time.Second)
		if err != nil {
			sitesCh <- res{err: err}
			return
		}
		sitesCh <- res{out: map[string]string{"raw": r.Stdout}}
	}()

	ovRes := <-ovCh
	if ovRes.err != nil {
		return nil, ovRes.err
	}
	stRes := <-sitesCh
	if stRes.err != nil {
		return nil, stRes.err
	}

	ov := ovRes.out
	siteLines, certMap := parseSitesScript(stRes.out["raw"])

	sites := make([]SiteEntry, 0, len(siteLines))
	for _, s := range siteLines {
		if s.PrimaryDomain != "" {
			if exp, ok := certMap[s.PrimaryDomain]; ok {
				s.CertExpiry = &exp
				if d, ok := daysUntil(exp); ok {
					s.CertDaysLeft = &d
				}
			}
		}
		sites = append(sites, s)
	}

	webVersion := ov["NGINX"]
	if ov["WEBSERVER"] == "apache" {
		webVersion = ov["APACHE"]
	}

	memParts := splitCSV(ov["MEM"], 3)
	diskParts := splitCSV(ov["DISK"], 3)
	dbFlavor := ""
	switch ov["DBFLAVOR"] {
	case "mysql":
		dbFlavor = "MySQL"
	case "mariadb":
		dbFlavor = "MariaDB"
	}

	overview := map[string]any{
		"memTotal":     toInt(memParts[0]),
		"memUsed":      toInt(memParts[1]),
		"memAvailable": toInt(memParts[2]),
		"diskTotal":    diskParts[0],
		"diskUsed":     diskParts[1],
		"diskPercent":  diskParts[2],
		"load":         ov["LOAD"],
		"cpu":          toInt(ov["CPU"]),
		"uptime":       ov["UPTIME"],
		"php":          ov["PHP"],
		"webServer":    ov["WEBSERVER"],
		"webVersion":   webVersion,
		"nginx":        ov["NGINX"],
		"apache":       ov["APACHE"],
		"mariadb":      ov["MYSQL"],
		"dbFlavor":     dbFlavor,
		"siteCount":    toInt(ov["SITECOUNT"]),
		"wpCount":      toInt(ov["WPCOUNT"]),
		"hasCertbot":   ov["CERTBOT"] == "1",
		"uid":          ov["WPSUDO"],
	}
	return &Snapshot{Overview: overview, Sites: sites, FetchedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}, nil
}

func parseKV(stdout string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		if idx := strings.Index(line, "="); idx > 0 {
			out[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
		}
	}
	return out
}

func parseSitesScript(stdout string) ([]SiteEntry, map[string]string) {
	var sites []SiteEntry
	certs := map[string]string{}
	mode := ""
	for _, raw := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch line {
		case "###SITES":
			mode = "sites"
			continue
		case "###CERTS":
			mode = "certs"
			continue
		case "###END":
			return sites, certs
		}
		if mode == "certs" {
			parts := strings.Split(line, "|")
			if len(parts) >= 3 && parts[1] != "" {
				certs[parts[1]] = parts[2]
			}
			continue
		}
		if mode != "sites" || !strings.HasPrefix(line, "SITE|") {
			continue
		}
		parts := strings.SplitN(line, "|", 9)
		if len(parts) < 8 {
			continue
		}
		domains := strings.Fields(parts[2])
		primary := ""
		if len(domains) > 0 {
			primary = domains[0]
		}
		t := parts[7]
		if t != "wordpress" && t != "custom" && t != "static" {
			t = ""
		}
		sites = append(sites, SiteEntry{
			File: parts[1], Domains: domains, PrimaryDomain: primary,
			Root: parts[3], SSL: parts[4] == "1", IsWordPress: parts[5] == "1",
			DBName: parts[6], Type: t,
		})
	}
	return sites, certs
}

func splitCSV(s string, n int) []string {
	parts := strings.Split(s, ",")
	for len(parts) < n {
		parts = append(parts, "")
	}
	return parts[:n]
}

func toInt(s string) int {
	var v int
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%d", &v)
	return v
}

var expiryRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func daysUntil(dateStr string) (int, bool) {
	if !expiryRe.MatchString(dateStr) {
		return 0, false
	}
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return 0, false
	}
	d := math.Round(float64(t.UTC().Sub(time.Now()) / (24 * time.Hour)))
	return int(d), true
}

// SiteRoot 只取站点配置里的真实根目录（nginx root / apache DocumentRoot）。
// 站点目录名不一定等于域名，凡是按域名拼 /var/www/<域名> 的地方都可能落空，
// 备份、删除这类按目录操作的功能应当先问这里。取不到时返回空串。
func SiteRoot(pool *sshc.Pool, server sshc.Server, domain string) string {
	script := `
f="/etc/nginx/sites-enabled/` + domain + `"
[ -e "$f" ] || f="/etc/apache2/sites-enabled/` + domain + `.conf"
[ -e "$f" ] || { exit 0; }
rootdir=$(awk '$1 == "root" { gsub(/;/, "", $2); print $2; exit }' "$f")
if [ -z "$rootdir" ]; then
  rootdir=$(awk '/^[ \t]*DocumentRoot[ \t]/ { sub(/^[ \t]*DocumentRoot[ \t]+/, ""); sub(/#.*/, ""); print; exit }' "$f" | tr -d '"')
fi
echo "ROOT=$rootdir"
`
	res, err := RunScript(pool, server, script, 15*time.Second)
	if err != nil || res.Code != 0 {
		return ""
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ROOT="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// GetSiteDetail 站点详情（脚本与早期实现一致）。
func GetSiteDetail(pool *sshc.Pool, server sshc.Server, domain string) (map[string]any, error) {
	script := DBPrelude + `
f="/etc/nginx/sites-enabled/` + domain + `"
[ -e "$f" ] || f="/etc/apache2/sites-enabled/` + domain + `.conf"
[ -e "$f" ] || { echo "ERR=notfound"; exit 0; }
# root 与 DocumentRoot 都要试：文件可能来自另一个 Web 服务器时期
rootdir=$(awk '$1 == "root" { gsub(/;/, "", $2); print $2; exit }' "$f")
if [ -z "$rootdir" ]; then
  rootdir=$(awk '/^[ \t]*DocumentRoot[ \t]/ { sub(/^[ \t]*DocumentRoot[ \t]+/, ""); sub(/#.*/, ""); print; exit }' "$f" | tr -d '"')
fi
echo "ROOT=$rootdir"

TMPD=$(mktemp -d)
du -sh "$rootdir" > "$TMPD/size" 2>/dev/null &
find "$rootdir" -type f > "$TMPD/files" 2>/dev/null &
if [ -f "$rootdir/wp-config.php" ] && [ -f "$rootdir/wp-settings.php" ]; then
  echo "WP=1"
  db=$(awk -F"'" '/DB_NAME/ {print $4; exit}' "$rootdir/wp-config.php")
  prefix=$(awk -F"'" '/table_prefix/ {print $2; exit}' "$rootdir/wp-config.php")
  echo "DB=$db"
  echo "WPVERSION=$(grep -m1 'wp_version =' "$rootdir/wp-includes/version.php" 2>/dev/null | cut -d"'" -f2)"
  if [ -n "$db" ] && [ -n "$prefix" ] && [ -n "$DB_CLI" ]; then
    prefix_safe=$(printf '%s' "$prefix" | tr -cd 'A-Za-z0-9_')
    theme=$("$DB_CLI" -N -B "$db" -e "SELECT option_value FROM ${prefix_safe}options WHERE option_name='template' LIMIT 1" 2>/dev/null)
    [ -n "$theme" ] && echo "THEME=$theme"
  fi
  plugins=$(find "$rootdir/wp-content/plugins" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l | tr -d ' ')
  [ "$plugins" != "0" ] && echo "PLUGINS=$plugins"
else
  echo "WP=0"
  if [ -f "/root/wp-` + domain + `-info.txt" ]; then
    db=$(awk -F'：' '/^数据库名/ {gsub(/[ \t]/, "", $2); print $2; exit}' "/root/wp-` + domain + `-info.txt" 2>/dev/null)
    [ -n "$db" ] && echo "DB=$db"
  fi
fi

primary=$(awk '/^[[:space:]]*server_name[[:space:]]/ {sub(/^[[:space:]]*server_name[[:space:]]+/, ""); gsub(/;/, ""); print $1; exit}' "$f")
stype=""
if [ -n "$primary" ]; then
  for tf in "/etc/wp-panel/site-types/$primary" "/etc/nginx/site-types/$primary"; do
    [ -f "$tf" ] || continue
    stype=$(head -1 "$tf" 2>/dev/null | tr -d '[:space:]')
  done
fi
if [ -z "$stype" ]; then
  if [ -f "$rootdir/wp-config.php" ] && [ -f "$rootdir/wp-settings.php" ]; then stype=wordpress
  elif grep -qE 'fastcgi_pass|proxy:unix:' "$f" 2>/dev/null; then stype=custom
  else stype=static; fi
fi
echo "TYPE=$stype"
if grep -qsE 'fastcgi_pass|proxy:unix:' "$f" "/etc/nginx/snippets/site-` + domain + `.conf"; then echo "PHP=on"; else echo "PHP=off"; fi
if grep -qs 'ssl_certificate ' "$f" 2>/dev/null || grep -qs 'SSLCertificateFile' "$f" 2>/dev/null; then echo "SSL=1"; else echo "SSL=0"; fi

wait
echo "SIZE=$(cut -f1 "$TMPD/size" 2>/dev/null)"
echo "FILES=$(wc -l < "$TMPD/files" 2>/dev/null | tr -d ' ')"
rm -rf "$TMPD"

echo "###SIZE"
du -sh "$rootdir"/wp-content/* 2>/dev/null | sort -h | tail -5
`
	res, err := RunScript(pool, server, script, 60*time.Second)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"raw": res.Stdout}
	keyRe := regexp.MustCompile(`^[A-Z_]+$`)
	for _, line := range strings.Split(res.Stdout, "\n") {
		idx := strings.Index(line, "=")
		if idx > 0 && keyRe.MatchString(line[:idx]) {
			out[line[:idx]] = line[idx+1:]
		}
	}
	return out, nil
}
