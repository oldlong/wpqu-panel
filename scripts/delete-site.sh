#!/usr/bin/env bash
#
# 删除一个站点。默认连配置、目录、数据库、证书一起清掉。
#
# 用法：
#   DOMAIN=blog.com bash delete-site.sh
#   DOMAIN=blog.com DELETE_FILES=0 DELETE_DB=1 bash delete-site.sh
#
# 环境变量：
#   DOMAIN        要删除的站点域名（必填，必须与配置里登记的域名一致）
#   DB_NAME       该站的数据库名，留空则跳过删库
#   DELETE_FILES  1 删除 /var/www/<域名>（默认 1）
#   DELETE_DB     1 删除数据库（默认 1，需 DB_NAME）
#   DELETE_CERT   1 删除 Let's Encrypt 证书（默认 1）
#
# 安全约定：
#   - 域名必须出现在配置的 server_name / ServerName 里，否则不动手
#   - 站点目录只能是 /var/www/<域名>，且不能是软链接
#   - 配置改动后先 nginx -t，失败就把备份放回去
#

set -Eeuo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "请用 root 运行：sudo -i 后重新执行" >&2
  exit 1
fi

trap 'echo -e "\n[ERROR] 第 $LINENO 行命令失败（退出码 $?），已中止。备份位于 ${TMPD:-无}" >&2' ERR

LOG="/var/log/wp-delsite-$(date +%Y%m%d-%H%M%S).log"
mkdir -p /var/log
exec > >(tee -a "$LOG") 2>&1

C_RESET='\033[0m'; C_INFO='\033[0;36m'; C_OK='\033[0;32m'; C_WARN='\033[0;33m'; C_ERR='\033[0;31m'
info() { echo -e "${C_INFO}[*]${C_RESET} $*"; }
ok()   { echo -e "${C_OK}[+]${C_RESET} $*"; }
warn() { echo -e "${C_WARN}[!]${C_RESET} $*"; }
die()  { echo -e "${C_ERR}[x]${C_RESET} $*" >&2; exit 1; }
section() { echo ""; echo -e "${C_INFO}── $* ──${C_RESET}"; }

# >>> ws-lib BEGIN
# ---------------------------------------------------------------- Web 服务器适配层
#
# 建站 / 换域名 / 删站 / 类型转换 / PHP 版本管理都要跟「当前这台机器用的是
# Nginx 还是 Apache」打交道。两边差异集中在几处：
#   - 配置在 sites-available/<域名>          vs  sites-available/<域名>.conf
#   - 域名      server_name a b;             vs  ServerName / ServerAlias
#   - 目录      root /path;                  vs  DocumentRoot /path
#   - PHP socket fastcgi_pass unix:/x.sock;  vs  SetHandler "proxy:unix:/x.sock|fcgi://localhost"
#   - 校验/重载 nginx -t / nginx             vs  apache2ctl configtest / apache2
#   - 证书      certbot --nginx              vs  certbot --apache
#
# 本段是从 _ws-lib.sh 注入的副本（各脚本要能单文件独立运行，所以不做 source）。
# 需要改逻辑：改 _ws-lib.sh，再执行
#     bash scripts/sync-ws-lib.sh
# 直接改下面这段，下次注入就会被覆盖。

WS=""
WS_SERVICE=""
WS_AVAIL=""
WS_ENABLED=""
WS_LOG_DIR=""
WS_CERT_PLUGIN=""
WS_SUFFIX=""
# 站点类型标记：原先放 /etc/nginx/site-types，只有 Nginx 机器会写。
# 改成与 Web 服务器无关的目录，换服务器类型也不会丢；旧位置继续读，读到就迁移。
WS_TYPE_DIR="/etc/wp-panel/site-types"
WS_LEGACY_TYPE_DIR="/etc/nginx/site-types"

ws_detect() {
  if systemctl is-active --quiet nginx 2>/dev/null; then
    printf 'nginx'
  elif systemctl is-active --quiet apache2 2>/dev/null; then
    printf 'apache'
  elif command -v nginx >/dev/null 2>&1; then
    printf 'nginx'
  elif command -v apache2ctl >/dev/null 2>&1; then
    printf 'apache'
  else
    printf 'none'
  fi
}

# ws_init：确定当前 Web 服务器并填好路径变量。两个都没装时返回 1。
# 需要强制指定时调用前设 WS_FORCE=nginx|apache。
ws_init() {
  WS="${WS_FORCE:-$(ws_detect)}"
  case "$WS" in
    nginx)
      WS_SERVICE="nginx"
      WS_AVAIL="/etc/nginx/sites-available"
      WS_ENABLED="/etc/nginx/sites-enabled"
      WS_LOG_DIR="/var/log/nginx"
      WS_CERT_PLUGIN="--nginx"
      WS_SUFFIX=""
      ;;
    apache)
      WS_SERVICE="apache2"
      WS_AVAIL="/etc/apache2/sites-available"
      WS_ENABLED="/etc/apache2/sites-enabled"
      WS_LOG_DIR="/var/log/apache2"
      WS_CERT_PLUGIN="--apache"
      WS_SUFFIX=".conf"
      ;;
    *)
      return 1
      ;;
  esac
  return 0
}

ws_name() { if [[ "$WS" == "apache" ]]; then printf 'Apache'; else printf 'Nginx'; fi; }
ws_conf() { printf '%s/%s%s' "$WS_AVAIL" "$1" "$WS_SUFFIX"; }
ws_link() { printf '%s/%s%s' "$WS_ENABLED" "$1" "$WS_SUFFIX"; }

# 站点配置里登记的所有域名（空格分隔，已去重、去引号）
ws_names() {
  local conf="$1" raw=""
  if [[ "$WS" == "apache" ]]; then
    raw=$(awk '
      /^[[:space:]]*ServerName[[:space:]]/  { sub(/^[[:space:]]*ServerName[[:space:]]+/, "");  sub(/#.*$/, ""); print; next }
      /^[[:space:]]*ServerAlias[[:space:]]/ { sub(/^[[:space:]]*ServerAlias[[:space:]]+/, ""); sub(/#.*$/, ""); print }
    ' "$conf" 2>/dev/null)
  else
    raw=$(awk '/^[[:space:]]*server_name[[:space:]]/ { sub(/^[[:space:]]*server_name[[:space:]]+/, ""); gsub(/;/, " "); print }' "$conf" 2>/dev/null)
  fi
  printf '%s' "$raw" | tr -d '"' | tr '\n' ' ' \
    | tr -s ' ' | sed 's/^ //; s/ $//' \
    | tr ' ' '\n' | awk 'NF && !seen[$0]++' | tr '\n' ' ' | sed 's/ $//'
}

# ws_has_name <conf> <domain>
ws_has_name() {
  local names
  names=" $(ws_names "$1") "
  # 不用 grep -q：pipefail 下早退会把整条管道判成失败
  case "$names" in
    *" $2 "*) return 0 ;;
  esac
  return 1
}

# ws_lookup <domain>：按文件名找配置，找不到再按配置里的域名反查
ws_lookup() {
  local d="$1" c f
  c=$(ws_conf "$d")
  if [[ -e "$c" ]]; then printf '%s' "$c"; return 0; fi
  c=$(ws_link "$d")
  if [[ -e "$c" ]]; then readlink -f "$c"; return 0; fi
  for f in "$WS_AVAIL"/*; do
    [[ -f "$f" ]] || continue
    if ws_has_name "$f" "$d"; then printf '%s' "$f"; return 0; fi
  done
  return 1
}

# ws_root_of <conf>：root / DocumentRoot
ws_root_of() {
  local conf="$1" r=""
  if [[ "$WS" == "apache" ]]; then
    r=$(awk '/^[[:space:]]*DocumentRoot[[:space:]]/ { sub(/^[[:space:]]*DocumentRoot[[:space:]]+/, ""); sub(/#.*$/, ""); print; exit }' "$conf" 2>/dev/null)
  else
    r=$(awk '$1 == "root" { gsub(/;/, "", $2); print $2; exit }' "$conf" 2>/dev/null)
  fi
  r=$(printf '%s' "$r" | tr -d '"')
  printf '%s' "${r%/}"
}

# ws_socket_of <conf> [更多文件...]：站点用的 php-fpm socket（没有则输出空）
ws_socket_of() {
  local f s=""
  for f in "$@"; do
    [[ -n "$s" ]] && break
    [[ -f "$f" ]] || continue
    if [[ "$WS" == "apache" ]]; then
      # grep 无匹配会返回 1，set -e + pipefail 下会把赋值判失败，必须兜住
      s=$(grep -o 'proxy:unix:[^|"]*' "$f" 2>/dev/null | sed -n '1p' || true)
      s="${s#proxy:unix:}"
    else
      s=$(awk '/^[[:space:]]*fastcgi_pass[[:space:]]/ { print $2; exit }' "$f" 2>/dev/null)
      s="${s#unix:}"
    fi
    s="${s%;}"
  done
  printf '%s' "$s"
}

# ws_test：配置校验。失败时把报错打到 stdout，返回 1
ws_test() {
  local out=""
  if [[ "$WS" == "apache" ]]; then
    if ! out=$(apache2ctl configtest 2>&1); then printf '%s' "$out"; return 1; fi
  else
    if ! out=$(nginx -t 2>&1); then printf '%s' "$out"; return 1; fi
  fi
  return 0
}

ws_reload() {
  systemctl reload "$WS_SERVICE" 2>/dev/null || systemctl restart "$WS_SERVICE"
}

ws_enable()  { ln -sfn "$(ws_conf "$1")" "$(ws_link "$1")"; }
ws_disable() { rm -f "$(ws_link "$1")"; }

ws_type_file() { printf '%s/%s' "$WS_TYPE_DIR" "$1"; }

# ws_type_of <domain> <conf> <root>
ws_type_of() {
  local d="$1" conf="$2" root="$3" t="" f
  for f in "$(ws_type_file "$d")" "$WS_LEGACY_TYPE_DIR/$d"; do
    [[ -f "$f" ]] || continue
    t=$(sed -n '1p' "$f" 2>/dev/null | tr -d '[:space:]')
    case "$t" in
      wordpress|custom|static) printf '%s' "$t"; return 0 ;;
    esac
    t=""
  done
  if [[ -n "$root" && -f "$root/wp-settings.php" && -f "$root/wp-config.php" ]]; then
    printf 'wordpress'
  elif [[ -n "$(ws_socket_of "$conf")" ]]; then
    printf 'custom'
  else
    printf 'static'
  fi
}

ws_type_write() {
  local d="$1" t="$2"
  install -d -m 755 "$WS_TYPE_DIR"
  printf '%s\n' "$t" > "$(ws_type_file "$d")"
  chmod 644 "$(ws_type_file "$d")"
  # 旧位置（Nginx 专用目录）读到就搬走，避免两处标记不一致
  if [[ -e "$WS_LEGACY_TYPE_DIR/$d" ]]; then rm -f "$WS_LEGACY_TYPE_DIR/$d"; fi
  return 0
}

# 已启用的站点配置列表（跳过 Debian 自带的 000-default）
ws_list_confs() {
  local f b
  for f in "$WS_ENABLED"/*; do
    [[ -e "$f" ]] || continue
    b=$(basename "$f")
    case "$b" in
      000-default.conf|default|default.conf) continue ;;
    esac
    printf '%s\n' "$f"
  done
}

# Apache vhost 里类型相关的公共指令（:80 与 :443 两块共用）
_ws_apache_common() {
  local root="$1" type="$2" sock="$3"
  printf '    DocumentRoot %s\n' "$root"
  printf '\n'
  printf '    <Directory %s>\n' "$root"
  printf '        Options -Indexes +FollowSymLinks\n'
  printf '        # WordPress 的固定链接、常见框架的伪静态都靠 .htaccess\n'
  printf '        AllowOverride All\n'
  printf '        Require all granted\n'
  printf '    </Directory>\n'
  printf '\n'
  case "$type" in
    static)
      printf '    DirectoryIndex index.html index.htm\n'
      printf '\n'
      printf '    # 静态站不解析 PHP：目录里即便混进 .php 也直接 403\n'
      printf '    <FilesMatch "\\.php$">\n'
      printf '        Require all denied\n'
      printf '    </FilesMatch>\n'
      ;;
    wordpress)
      printf '    DirectoryIndex index.php index.html\n'
      printf '\n'
      printf '    # PHP 交给 php-fpm 的 unix socket（不用 mod_php，省内存）\n'
      printf '    <FilesMatch "\\.php$">\n'
      printf '        SetHandler "proxy:unix:%s|fcgi://localhost"\n' "$sock"
      printf '    </FilesMatch>\n'
      printf '\n'
      printf '    <FilesMatch "^(wp-config\\.php|readme\\.html)$">\n'
      printf '        Require all denied\n'
      printf '    </FilesMatch>\n'
      printf '\n'
      printf '    <FilesMatch "^\\.ht">\n'
      printf '        Require all denied\n'
      printf '    </FilesMatch>\n'
      ;;
    *)
      printf '    DirectoryIndex index.php index.html\n'
      printf '\n'
      printf '    <FilesMatch "\\.php$">\n'
      printf '        SetHandler "proxy:unix:%s|fcgi://localhost"\n' "$sock"
      printf '    </FilesMatch>\n'
      ;;
  esac
}

# Apache vhost 里已有的证书路径（certbot --apache 写进去的可能是 -0001 后缀）
_ws_apache_cert() {
  local conf="$1" cert="" key=""
  if [[ -f "$conf" ]]; then
    cert=$(awk '/^[[:space:]]*SSLCertificateFile[[:space:]]/ { print $2; exit }' "$conf" 2>/dev/null | tr -d '"')
    key=$(awk '/^[[:space:]]*SSLCertificateKeyFile[[:space:]]/ { print $2; exit }' "$conf" 2>/dev/null | tr -d '"')
  fi
  printf '%s|%s' "$cert" "$key"
}

# ws_apache_write_vhost <domain> <root> <type> <sock> <conf> [cert] [key]
# 整份重写 vhost。有证书时同时写 :80（301 到 https）与 :443 两块，
# 这样切换站点类型不会把 certbot 装好的证书弄丢。
ws_apache_write_vhost() {
  local domain="$1" root="$2" type="$3" sock="$4" conf="$5"
  local cert="${6:-}" key="${7:-}" ssl=0

  # 手工装过 Apache 的机器可能没开 proxy_fcgi / rewrite，SetHandler 会直接校验失败。
  # a2enmod 是幂等的，开完重载即生效。
  if command -v a2enmod >/dev/null 2>&1; then
    a2enmod -q proxy proxy_fcgi setenvif rewrite headers expires >/dev/null 2>&1 || true
  fi

  if [[ -n "$cert" && -f "$cert" && -n "$key" && -f "$key" ]]; then
    ssl=1
  fi

  {
    printf '# 由 WordPress 管理面板生成（%s）\n' "$(date '+%Y-%m-%d %H:%M:%S')"
    printf '# 站点能力（PHP / 静态 / WordPress）写在 VirtualHost 内，切换类型会整份重写。\n'
    printf '# SSL 部分按证书路径重建，certbot --apache 申请过证书的站点不会因此掉 HTTPS。\n'
    printf '\n'
    printf '<VirtualHost *:80>\n'
    printf '    ServerName %s\n' "$domain"
    printf '    ServerAlias www.%s\n' "$domain"
    _ws_apache_common "$root" "$type" "$sock"
    if [[ "$ssl" == "1" ]]; then
      printf '\n'
      printf '    # 证书续期走 /.well-known，不能被 301 掉\n'
      printf '    RewriteEngine On\n'
      printf '    RewriteCond %%{REQUEST_URI} !^/\\.well-known/\n'
      printf '    RewriteRule ^ https://%%{HTTP_HOST}%%{REQUEST_URI} [L,R=301]\n'
    fi
    printf '\n'
    printf '    ErrorLog  %s/%s.error.log\n' "$WS_LOG_DIR" "$domain"
    printf '    CustomLog %s/%s.access.log combined\n' "$WS_LOG_DIR" "$domain"
    printf '</VirtualHost>\n'

    if [[ "$ssl" == "1" ]]; then
      printf '\n'
      printf '<VirtualHost *:443>\n'
      printf '    ServerName %s\n' "$domain"
      printf '    ServerAlias www.%s\n' "$domain"
      _ws_apache_common "$root" "$type" "$sock"
      printf '\n'
      printf '    SSLEngine on\n'
      printf '    SSLCertificateFile %s\n' "$cert"
      printf '    SSLCertificateKeyFile %s\n' "$key"
      if [[ -f /etc/letsencrypt/options-ssl-apache.conf ]]; then
        printf '    Include /etc/letsencrypt/options-ssl-apache.conf\n'
      fi
      printf '\n'
      printf '    ErrorLog  %s/%s.error.log\n' "$WS_LOG_DIR" "$domain"
      printf '    CustomLog %s/%s.access.log combined\n' "$WS_LOG_DIR" "$domain"
      printf '</VirtualHost>\n'
    fi
  } > "$conf"
  chmod 644 "$conf"
}

# WordPress 的伪静态规则。wp rewrite flush --hard 在 CLI 下不写文件
# （它靠 $_SERVER['SERVER_SOFTWARE'] 判断，命令行里为空），所以直接落一份标准规则。
ws_write_wp_htaccess() {
  local root="$1"
  cat > "${root}/.htaccess" <<'HTACCESS'
# BEGIN WordPress
<IfModule mod_rewrite.c>
RewriteEngine On
RewriteBase /
RewriteRule ^index\.php$ - [L]
RewriteCond %{REQUEST_FILENAME} !-f
RewriteCond %{REQUEST_FILENAME} !-d
RewriteRule . /index.php [L]
</IfModule>
# END WordPress
HTACCESS
  chown www-data:www-data "${root}/.htaccess"
  chmod 644 "${root}/.htaccess"
}

# 把站点目录里的 .htaccess 挪开（切到静态/自助网站时用，避免规则重写到 index.php）
ws_park_htaccess() {
  local root="$1" stamp
  [[ -f "${root}/.htaccess" ]] || return 0
  grep -qs 'BEGIN WordPress' "${root}/.htaccess" || return 0
  stamp=$(date +%Y%m%d-%H%M%S)
  mv "${root}/.htaccess" "${root}/.htaccess.wp-bak-${stamp}"
  return 0
}
# <<< ws-lib END


DOMAIN="${DOMAIN:-}"
DB_NAME="${DB_NAME:-}"
DELETE_FILES="${DELETE_FILES:-1}"
DELETE_DB="${DELETE_DB:-1}"
DELETE_CERT="${DELETE_CERT:-1}"

DOMAIN="${DOMAIN#http://}"; DOMAIN="${DOMAIN#https://}"; DOMAIN="${DOMAIN%%/*}"
[[ -n "$DOMAIN" ]] || die "必须提供 DOMAIN"

if ! [[ "$DOMAIN" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$ ]]; then
  die "域名格式不合法：${DOMAIN}"
fi

if ! ws_init; then
  die "既没有运行中的 Nginx，也没有 Apache，无法定位站点配置"
fi
WS_NAME=$(ws_name)

WEB_ROOT="/var/www/${DOMAIN}"
NGINX_CONF=$(ws_conf "$DOMAIN")
# 站点目录以配置里的 root / DocumentRoot 为准：目录名不一定等于域名
# （例如 pc5000.com 的 root 是 /var/www/pc5000），按域名硬拼会漏删真实目录。
CONF_ROOT=$(ws_root_of "$NGINX_CONF" 2>/dev/null || true)
if [[ -n "$CONF_ROOT" && -d "$CONF_ROOT" ]]; then
  WEB_ROOT="${CONF_ROOT%/}"
fi
NGINX_LINK=$(ws_link "$DOMAIN")
SNIPPET="/etc/nginx/snippets/site-${DOMAIN}.conf"
TYPE_FILE=$(ws_type_file "$DOMAIN")
INFO_FILE="/root/wp-${DOMAIN}-info.txt"

TMPD="$(mktemp -d /tmp/wp-delsite.XXXXXX)"
BK="${TMPD}/backup"
mkdir -p "$BK"

echo ""
echo "  站点：${DOMAIN}"
echo "  目录：${WEB_ROOT}"
echo "  备份：${BK}"
echo ""

# ---------------------------------------------------------------- 前置校验

section "校验"

[[ -e "$NGINX_LINK" || -e "$NGINX_CONF" ]] || die "找不到 ${DOMAIN} 的 ${WS_NAME} 配置，已中止（可能是别的面板管的站）"

if [[ -e "$NGINX_CONF" ]]; then
  NAMES=$(ws_names "$NGINX_CONF")
  ws_has_name "$NGINX_CONF" "$DOMAIN" \
    || die "配置里登记的域名（${NAMES:-空}）不含 ${DOMAIN}，已中止"
  ok "域名匹配：${NAMES}"
else
  warn "只有 sites-enabled 里的链接，没有 sites-available 配置"
fi

# 先记下这个站用的 PHP socket，稍后判断 pool 要不要一起删
PHP_SOCK=$(ws_socket_of "$NGINX_CONF" "$SNIPPET")

# ---------------------------------------------------------------- 站点配置

section "移除 ${WS_NAME} 配置"

HAD_CONF=0
[[ -e "$NGINX_CONF" ]] && HAD_CONF=1

for f in "$NGINX_CONF" "$SNIPPET" "$TYPE_FILE" "$WS_LEGACY_TYPE_DIR/$DOMAIN"; do
  if [[ -e "$f" ]]; then
    cp -a "$f" "$BK/" 2>/dev/null || true
    info "备份 $(basename "$f")"
  fi
done

ws_disable "$DOMAIN"
ok "已停用 $(basename "$NGINX_LINK")"

if ! WS_TEST=$(ws_test 2>&1); then
  # 配置检查不过就立刻把链接放回去，别把整台服务器留在坏状态
  if [[ "$HAD_CONF" == "1" ]]; then
    ln -sfn "$NGINX_CONF" "$NGINX_LINK" 2>/dev/null || true
  fi
  echo "$WS_TEST" >&2
  die "${WS_NAME} 配置校验未通过，已恢复站点启用状态。请检查其他站点的配置是否引用了本站"
fi

rm -f "$NGINX_CONF" "$SNIPPET" "$TYPE_FILE"
ok "已删除站点配置与能力片段"

if systemctl is-active --quiet "$WS_SERVICE"; then
  if ws_reload; then
    ok "${WS_NAME} 已重新加载"
  else
    warn "${WS_NAME} 重新加载失败，请手工检查配置"
  fi
fi

# ---------------------------------------------------------------- PHP pool

section "处理 PHP-FPM pool"

POOL_REMOVED=0
if [[ -n "$PHP_SOCK" ]]; then
  REF=$(grep -Rl -- "${PHP_SOCK}" "$WS_ENABLED" /etc/nginx/snippets 2>/dev/null | wc -l | tr -d ' ')
  if [[ "${REF:-0}" -gt 0 ]]; then
    info "socket ${PHP_SOCK} 仍被 ${REF} 个配置引用，保留 pool（共享 worker）"
  else
    PHP_VER=$(ls /etc/php 2>/dev/null | sort -V | tail -1 || true)
    POOL_FILE=""
    if [[ -n "$PHP_VER" && -d "/etc/php/${PHP_VER}/fpm/pool.d" ]]; then
      POOL_FILE=$(grep -l "listen[[:space:]]*=[[:space:]]*${PHP_SOCK}" "/etc/php/${PHP_VER}/fpm/pool.d/"*.conf 2>/dev/null | head -1 || true)
    fi
    if [[ -n "$POOL_FILE" && -f "$POOL_FILE" ]]; then
      cp -a "$POOL_FILE" "$BK/" 2>/dev/null || true
      rm -f "$POOL_FILE"
      POOL_REMOVED=1
      ok "已删除独立 pool：${POOL_FILE}"
    else
      info "没找到对应的独立 pool 配置，跳过"
    fi
  fi
else
  info "该站未使用 PHP（无 fastcgi_pass），跳过"
fi

# ---------------------------------------------------------------- 数据库

section "数据库"

# 面板没传库名不等于「这个站没有库」：以前这里直接跳过，勾了删库却什么都没删，
# 库和账号就永久躺在机器上。现在自己从 wp-config.php 读一次。
DB_OWNER=""
if [[ "$DELETE_DB" == "1" && -z "$DB_NAME" && -f "${WEB_ROOT}/wp-config.php" ]]; then
  DB_NAME=$(awk -F"'" '/DB_NAME/ {print $4; exit}' "${WEB_ROOT}/wp-config.php" 2>/dev/null || true)
  DB_OWNER=$(awk -F"'" '/DB_USER/ {print $4; exit}' "${WEB_ROOT}/wp-config.php" 2>/dev/null || true)
  [[ -n "$DB_NAME" ]] && info "从 wp-config.php 读到库名：${DB_NAME}（账号 ${DB_OWNER:-未知}）"
fi

if [[ "$DELETE_DB" == "1" ]]; then
  if [[ -z "$DB_NAME" ]]; then
    warn "没有提供数据库名，也没有 wp-config.php 可读，跳过删库"
  elif ! [[ "$DB_NAME" =~ ^[A-Za-z0-9_]+$ ]]; then
    warn "数据库名不合法（${DB_NAME}），跳过"
  elif [[ "$DB_NAME" =~ ^(mysql|information_schema|performance_schema|sys)$ ]]; then
    warn "拒绝删除系统库 ${DB_NAME}"
  else
    DB_CLI=$(command -v mariadb || command -v mysql || true)
    if [[ -z "$DB_CLI" ]]; then
      warn "找不到 mysql / mariadb 客户端，跳过删库（可稍后在数据库页手工删除）"
    else
      if "$DB_CLI" -e "DROP DATABASE IF EXISTS \`${DB_NAME}\`;" 2>"${TMPD}/dberr"; then
        ok "数据库 ${DB_NAME} 已删除"
        # 顺手清掉这个站专用的库账号：只在它没有别的库权限时才删。
        # 老站（比如几个站共用 wordpress + wpuser）绝不能动，否则删一个站把邻居搞挂。
        if [[ -n "$DB_OWNER" && "$DB_OWNER" =~ ^[A-Za-z0-9_]+$ ]]; then
          OTHERS=$("$DB_CLI" -N -B -e "SELECT COUNT(*) FROM information_schema.SCHEMA_PRIVILEGES WHERE GRANTEE = \"'${DB_OWNER}'@'localhost'\" AND TABLE_SCHEMA <> '${DB_NAME}'" 2>/dev/null || echo 0)
          case "$OTHERS" in '' | *[!0-9]*) OTHERS=0 ;; esac
          if [[ "$OTHERS" -gt 0 ]]; then
            info "账号 ${DB_OWNER} 还管着别的库，保留"
          elif "$DB_CLI" -e "DROP USER IF EXISTS '${DB_OWNER}'@'localhost';" 2>"${TMPD}/dberr"; then
            ok "账号 ${DB_OWNER} 已删除"
          else
            warn "删除账号 ${DB_OWNER} 失败：$(tr '\n' ' ' < "${TMPD}/dberr")"
          fi
        fi
      else
        warn "删除数据库失败：$(tr '\n' ' ' < "${TMPD}/dberr")"
      fi
    fi
  fi
else
  info "按设置保留数据库"
fi

# ---------------------------------------------------------------- 站点目录

section "站点目录"

if [[ "$DELETE_FILES" == "1" ]]; then
  if [[ "$WEB_ROOT" != /var/www/* || "$WEB_ROOT" == "/var/www" || "$WEB_ROOT" == "/var/www/" ]]; then
    die "目录路径异常（${WEB_ROOT}），拒绝删除"
  elif [[ -L "$WEB_ROOT" ]]; then
    warn "${WEB_ROOT} 是软链接，跳过（请手工确认它指向哪里）"
  elif [[ -d "$WEB_ROOT" ]]; then
    SIZE=$(du -sh "$WEB_ROOT" 2>/dev/null | cut -f1 || true)
    rm -rf -- "$WEB_ROOT"
    ok "已删除 ${WEB_ROOT}（${SIZE:-未知大小}）"
  else
    info "${WEB_ROOT} 不存在，跳过"
  fi
else
  info "按设置保留站点目录"
fi

# ---------------------------------------------------------------- 证书

section "证书"

if [[ "$DELETE_CERT" == "1" ]]; then
  if command -v certbot >/dev/null 2>&1; then
    if [[ -d "/etc/letsencrypt/live/${DOMAIN}" ]]; then
      if certbot delete --cert-name "$DOMAIN" --non-interactive >"${TMPD}/certbot" 2>&1; then
        ok "证书 ${DOMAIN} 已删除"
      else
        warn "删除证书失败：$(tail -3 "${TMPD}/certbot" | tr '\n' ' ')"
      fi
    else
      info "没有找到 ${DOMAIN} 的证书"
    fi
  else
    info "未安装 certbot，跳过"
  fi
else
  info "按设置保留证书"
fi

# ---------------------------------------------------------------- 收尾

rm -f "$INFO_FILE"

if [[ "$POOL_REMOVED" == "1" ]] && [[ -n "${PHP_VER:-}" ]]; then
  FPM_BIN=""
  for c in "php-fpm${PHP_VER}" "/usr/sbin/php-fpm${PHP_VER}"; do
    if command -v "$c" >/dev/null 2>&1; then FPM_BIN="$c"; break; fi
  done
  # 先校验再重载：一份坏 pool 会让 php-fpm 整体起不来，所有站点一起 502
  if [[ -n "$FPM_BIN" ]] && "$FPM_BIN" -t >/dev/null 2>&1; then
    systemctl reload "php${PHP_VER}-fpm" >/dev/null 2>&1 \
      || systemctl restart "php${PHP_VER}-fpm" >/dev/null 2>&1 \
      || warn "php-fpm 重新加载失败，请手工检查"
    ok "php${PHP_VER}-fpm 已重新加载"
  else
    warn "php-fpm 配置校验未通过，未重载；pool 已删除，必要时重启 php${PHP_VER}-fpm"
  fi
fi

section "完成"

REMAIN=$(ws_list_confs | wc -l | tr -d ' ')
ok "站点 ${DOMAIN} 已删除，当前还剩 ${REMAIN} 个站点"
echo "  本次操作日志：${LOG}"
echo "  配置备份：${BK}（确认无误后可删）"
