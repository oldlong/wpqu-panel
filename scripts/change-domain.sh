#!/usr/bin/env bash
#
# WordPress 站点域名变更脚本
#
# 适用：由 install-wordpress.sh 部署的站点（Nginx 或 Apache + PHP-FPM + 数据库）
# 用法：
#   OLD_DOMAIN=old.com NEW_DOMAIN=new.com bash change-domain.sh
#
# 可选环境变量：
#   OLD_DOMAIN      原域名（必填）
#   NEW_DOMAIN      新域名（必填）
#   ENABLE_SSL      1/0，默认 1，新域名解析正常时自动申请证书
#   KEEP_REDIRECT   1/0，默认 1，旧域名保留 301 跳转到新域名
#   MOVE_DIR        1/0，默认 1，网站目录 /var/www/old 一并改名为 /var/www/new
#   DRY_RUN         1 时只打印步骤，不做任何改动
#
# 执行顺序（顺序很关键）：
#   备份 → 目录改名 → 同步 PHP-FPM chdir → 写 Nginx 配置 → 申请证书
#   → 旧域名 301 → 数据库搜索替换 → 自检
#

set -Eeuo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "请用 root 运行：sudo -i 后重新执行" >&2
  exit 1
fi

trap 'echo -e "\n[ERROR] 第 $LINENO 行命令失败（退出码 $?），已中止。日志：${LOG:-未创建}" >&2' ERR

LOG="/var/log/wp-domain-change-$(date +%Y%m%d-%H%M%S).log"
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


# MariaDB / MySQL 8 的命令名不同（装 MySQL 时只有 mysql / mysqldump），统一探测
DB_CLI=$(command -v mariadb || command -v mysql || true)
DB_DUMP=$(command -v mariadb-dump || command -v mysqldump || true)

WP_BIN="/usr/local/bin/wp"
WP_CACHE="/tmp/wp-cli-cache"

OLD_DOMAIN="${OLD_DOMAIN:-}"
NEW_DOMAIN="${NEW_DOMAIN:-}"
ENABLE_SSL="${ENABLE_SSL:-1}"
KEEP_REDIRECT="${KEEP_REDIRECT:-1}"
MOVE_DIR="${MOVE_DIR:-1}"
DRY_RUN="${DRY_RUN:-0}"

if [[ -t 0 ]]; then
  if [[ -z "$OLD_DOMAIN" ]]; then
    read -r -p "当前域名（如 old.com）: " OLD_DOMAIN || true
  fi
  if [[ -z "$NEW_DOMAIN" ]]; then
    read -r -p "新域名（如 new.com）: " NEW_DOMAIN || true
  fi
fi

[[ -n "$OLD_DOMAIN" && -n "$NEW_DOMAIN" ]] || die "必须提供 OLD_DOMAIN 和 NEW_DOMAIN"
OLD_DOMAIN="${OLD_DOMAIN#http://}"; OLD_DOMAIN="${OLD_DOMAIN#https://}"; OLD_DOMAIN="${OLD_DOMAIN%%/*}"
NEW_DOMAIN="${NEW_DOMAIN#http://}"; NEW_DOMAIN="${NEW_DOMAIN#https://}"; NEW_DOMAIN="${NEW_DOMAIN%%/*}"
[[ "$OLD_DOMAIN" != "$NEW_DOMAIN" ]] || die "新旧域名相同，无需变更"

WEB_ROOT_OLD=""
NGINX_OLD=""
WEB_ROOT_NEW="/var/www/${NEW_DOMAIN}"
NGINX_NEW=""
BACKUP_DIR="/var/backups/wp-domain-$(date +%Y%m%d-%H%M%S)"

# ---------------------------------------------------------------- 前置检查

section "前置检查"

if ! ws_init; then
  die "既没有运行中的 Nginx，也没有 Apache，无法定位站点配置"
fi
WS_NAME=$(ws_name)
NGINX_NEW=$(ws_conf "$NEW_DOMAIN")

# 配置：优先按域名找，找不到再按配置里登记的域名反查（兼容 www 前缀、文件名与域名不一致）
NGINX_OLD=$(ws_lookup "$OLD_DOMAIN" 2>/dev/null || true)
if [[ -n "$NGINX_OLD" && "$(basename "$NGINX_OLD")" != "${OLD_DOMAIN}${WS_SUFFIX}" ]]; then
  warn "按域名没找到配置，已按配置里的域名定位到 $NGINX_OLD"
fi
[[ -n "$NGINX_OLD" && -f "$NGINX_OLD" ]] || die "找不到 ${OLD_DOMAIN} 的 ${WS_NAME} 配置。
    已检查：$(ws_conf "$OLD_DOMAIN") 与 ${WS_AVAIL} 下所有配置里登记的域名。
    如果站点名与目录名不一致，请确认站点卡片上显示的域名是配置里的哪一个。"

# 网站目录：以配置里的 root / DocumentRoot 为准，避免 www 前缀或目录名不一致导致误判
WEB_ROOT_OLD=$(ws_root_of "$NGINX_OLD")
if [[ -z "$WEB_ROOT_OLD" ]]; then
  WEB_ROOT_OLD="/var/www/${OLD_DOMAIN}"
fi
WEB_ROOT_OLD="${WEB_ROOT_OLD%/}"
ok "站点配置：$NGINX_OLD"
ok "网站目录：$WEB_ROOT_OLD"

if [[ ! -d "$WEB_ROOT_OLD" ]]; then
  die "网站目录 $WEB_ROOT_OLD 不存在（来自 $NGINX_OLD 的 root 指令）"
fi
if [[ ! -f "$WEB_ROOT_OLD/wp-config.php" ]]; then
  die "$WEB_ROOT_OLD 下没有 wp-config.php，这不是可变更域名的 WordPress 站点"
fi

# wp-cli 是变更域名的核心依赖（序列化安全替换），必须真的能跑。
# 只判断文件存在不够：下载失败留下的 0 字节文件执行时会静默成功。
wp_cli_ok() {
  [[ -s "$WP_BIN" ]] && php "$WP_BIN" --info --allow-root >/dev/null 2>&1
}
if ! wp_cli_ok; then
  if [[ "$DRY_RUN" == "1" ]]; then
    warn "wp-cli 不可用（$WP_BIN 缺失/0 字节/损坏），正式执行时会在数据库替换阶段中止"
  else
    die "wp-cli 不可用（$WP_BIN 缺失、为 0 字节或已损坏），变更域名必须用它做数据库替换。

    修复方法（任选其一）：
      1. 重新执行 install-wordpress.sh（会自动从镜像下载）
      2. 手动安装：
         curl -fsSL -o /usr/local/bin/wp \\
           https://ghfast.top/https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar
         chmod 755 /usr/local/bin/wp && wp --version --allow-root"
  fi
fi

# 站点必须已经完成 WordPress 安装，否则没有表可替换
if [[ "$DRY_RUN" != "1" ]] && wp_cli_ok; then
  if ! runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
        core is-installed --path="$WEB_ROOT_OLD" >/dev/null 2>&1; then
    die "$WEB_ROOT_OLD 还不是一个装好的 WordPress（数据库里没有表）。
    先用浏览器打开 http://${OLD_DOMAIN}/wp-admin/install.php 完成安装，再变更域名。
    这样也能避免把站点改到一半才发现数据库是空的。"
  fi
fi

if [[ "$WEB_ROOT_NEW" == "$WEB_ROOT_OLD" ]]; then
  MOVE_DIR=0
fi
if [[ "$MOVE_DIR" == "1" && -e "$WEB_ROOT_NEW" ]]; then
  if [[ -d "$WEB_ROOT_NEW" && -z "$(ls -A "$WEB_ROOT_NEW" 2>/dev/null)" ]]; then
    rmdir "$WEB_ROOT_NEW" 2>/dev/null || true
  else
    die "目录 $WEB_ROOT_NEW 已存在且非空，请先处理（或取消“网站目录一并改名”）"
  fi
fi
if [[ -e "$NGINX_NEW" ]]; then
  die "Nginx 配置 $NGINX_NEW 已存在，请先处理（rm -f $NGINX_NEW）"
fi

# PHP 版本：优先 CLI，缺失时从已安装目录推断
PHP_VER=""
if command -v php >/dev/null 2>&1; then
  PHP_VER=$(php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null || true)
fi
if [[ -z "$PHP_VER" ]]; then
  PHP_VER=$(ls -1d /etc/php/*/fpm 2>/dev/null | awk -F/ '{print $(NF-1)}' | sort -V | awk 'END{print}')
fi
if [[ -z "$PHP_VER" ]]; then
  die "无法检测 PHP 版本（没有 php 命令，也没有 /etc/php/*/fpm）"
fi

# 从现有配置解析 socket，避免猜错。
# Nginx 的能力规则在 snippets/site-<域名>.conf 里，fastcgi_pass 可能只出现在那里；
# Apache 则写在 VirtualHost 的 SetHandler "proxy:unix:...|fcgi://localhost"。
PHP_SOCK=$(ws_socket_of "$NGINX_OLD" "/etc/nginx/snippets/site-${OLD_DOMAIN}.conf")
if [[ -z "$PHP_SOCK" ]]; then
  PHP_SOCK="/run/php/php${PHP_VER}-fpm-wordpress.sock"
  warn "未能从配置中解析 socket，使用默认值 $PHP_SOCK"
fi

ok "旧站：$WEB_ROOT_OLD"
ok "PHP：$PHP_VER   socket：$PHP_SOCK"

NEW_RESOLVED=0
if getent hosts "$NEW_DOMAIN" >/dev/null 2>&1; then
  NEW_RESOLVED=1
  ok "新域名已解析：$(getent hosts "$NEW_DOMAIN" | awk 'NR==1 {print $1}')"
else
  warn "新域名 $NEW_DOMAIN 尚未解析，本次跳过 HTTPS"
  ENABLE_SSL=0
fi

if [[ "$DRY_RUN" == "1" ]]; then
  warn "DRY_RUN=1，只打印步骤，不执行改动"
fi

# ---------------------------------------------------------------- 备份

section "备份"

if [[ "$DRY_RUN" == "1" ]]; then
  info "将备份到 $BACKUP_DIR"
else
  mkdir -p "$BACKUP_DIR"
  chmod 700 "$BACKUP_DIR"

  mkdir -p "$WP_CACHE"
  chown www-data:www-data "$WP_CACHE"

  if [[ -x "$WP_BIN" ]]; then
    # www-data 无权写 700 的备份目录，先导出到 /tmp 再移动
    runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
      db export /tmp/wp-domain-export.sql --path="$WEB_ROOT_OLD" >/dev/null
    mv /tmp/wp-domain-export.sql "$BACKUP_DIR/database.sql"
    chmod 600 "$BACKUP_DIR/database.sql"
    ok "数据库已导出：$BACKUP_DIR/database.sql"
  else
    db_name=$(awk '/DB_NAME/ {print $3}' "$WEB_ROOT_OLD/wp-config.php" | tr -d "');")
    "$DB_DUMP" "$db_name" > "$BACKUP_DIR/database.sql"
    chmod 600 "$BACKUP_DIR/database.sql"
    ok "数据库已导出（$(basename "$DB_DUMP")）：$BACKUP_DIR/database.sql"
  fi

  cp -a "$WEB_ROOT_OLD/wp-config.php" "$BACKUP_DIR/wp-config.php.bak"
  cp -a "$NGINX_OLD" "$BACKUP_DIR/site-${OLD_DOMAIN}.conf.bak"
  ok "配置已备份到 $BACKUP_DIR"
  warn "上传的图片等站点文件不在本次备份内，建议另行 tar 归档 wp-content"
fi

# ---------------------------------------------------------------- 目录改名

section "调整网站目录"

if [[ "$MOVE_DIR" == "1" ]]; then
  if [[ "$DRY_RUN" == "1" ]]; then
    info "将执行：mv $WEB_ROOT_OLD $WEB_ROOT_NEW"
    WEB_ROOT="$WEB_ROOT_NEW"
  else
    mv "$WEB_ROOT_OLD" "$WEB_ROOT_NEW"
    chown -R www-data:www-data "$WEB_ROOT_NEW"
    WEB_ROOT="$WEB_ROOT_NEW"
    ok "目录已改名：$WEB_ROOT_NEW"
  fi

  # ------------------------------------------------------------ PHP-FPM chdir
  section "同步 PHP-FPM 工作目录"

  if [[ "$DRY_RUN" == "1" ]]; then
    info "若 $PHP_SOCK 是本站独占的 pool，会把它的 chdir 改为 $WEB_ROOT"
    info "将重载 php${PHP_VER}-fpm"
  else
    # pool 里的 chdir 还指向旧路径，不改会导致 PHP 请求失败。
    # 但共享池（多个站点共用一个 socket）不能跟着某个站点改：那个目录一旦被删，
    # php-fpm 会因 chdir 不存在而整份配置校验失败，所有站点一起 502。
    # sites-enabled 里是软链，grep -r 不跟进软链会漏算，必须用 -R
    REF_COUNT=$(grep -Rl "fastcgi_pass unix:${PHP_SOCK}" /etc/nginx/sites-enabled /etc/nginx/snippets 2>/dev/null | wc -l | tr -d ' ')
    if [[ "$REF_COUNT" -gt 1 ]]; then
      warn "socket $PHP_SOCK 被 ${REF_COUNT} 个站点共用（共享池），chdir 保持不动"
    else
      POOL_FILES=$(grep -rl "listen = ${PHP_SOCK}" /etc/php/*/fpm/pool.d/ 2>/dev/null || true)
      if [[ -z "$POOL_FILES" ]]; then
        warn "未找到使用 $PHP_SOCK 的 pool 配置，若站点报错请手动检查 chdir"
      else
        while IFS= read -r f; do
          [[ -n "$f" ]] || continue
          if grep -q "^chdir = " "$f"; then
            sed -i "s|^chdir = .*|chdir = ${WEB_ROOT}|" "$f"
          else
            printf '\nchdir = %s\n' "$WEB_ROOT" >> "$f"
          fi
          ok "chdir 已更新：$f"
        done <<< "$POOL_FILES"
      fi
    fi

    # chdir 变更需要新 worker 才生效，reload 比 restart 平滑
    if ! systemctl reload "php${PHP_VER}-fpm"; then
      warn "reload 失败，改用 restart"
      systemctl restart "php${PHP_VER}-fpm"
    fi
    ok "PHP-FPM 已重载"
  fi
else
  WEB_ROOT="$WEB_ROOT_OLD"
  warn "保持原目录 $WEB_ROOT，仅变更访问域名，PHP-FPM 配置无需调整"
fi

# ---------------------------------------------------------------- Nginx

section "写入站点配置"

if [[ "$DRY_RUN" == "1" ]]; then
  info "将写入 $NGINX_NEW（root=$WEB_ROOT，socket=$PHP_SOCK）"
  info "将启用 ${NEW_DOMAIN} 并停用 ${OLD_DOMAIN}"
  info "将执行 ${WS_NAME} 配置校验并重载"
else

if [[ "$WS" == "apache" ]]; then
  # Apache 的能力指令写在 VirtualHost 内，整份重写；类型沿用原站点
  SITE_TYPE=$(ws_type_of "$OLD_DOMAIN" "$NGINX_OLD" "$WEB_ROOT")
  info "站点类型：${SITE_TYPE}（沿用原站点）"

  ws_apache_write_vhost "$NEW_DOMAIN" "$WEB_ROOT" "$SITE_TYPE" "$PHP_SOCK" "$NGINX_NEW"
  ws_type_write "$NEW_DOMAIN" "$SITE_TYPE"
  if [[ "$SITE_TYPE" == "wordpress" ]]; then
    ws_write_wp_htaccess "$WEB_ROOT"
  fi

  OLD_LINK=$(ws_link "$OLD_DOMAIN")
  ws_enable "$NEW_DOMAIN"
  rm -f "$OLD_LINK"

  if ! WS_TEST=$(ws_test); then
    ws_disable "$NEW_DOMAIN"
    rm -f "$NGINX_NEW"
    if [[ -n "$OLD_LINK" ]]; then
      ln -sfn "$NGINX_OLD" "$OLD_LINK"
    else
      ln -sfn "$NGINX_OLD" "$(ws_link "$OLD_DOMAIN")"
    fi
    ws_reload >/dev/null 2>&1 || true
    if [[ "$MOVE_DIR" == "1" && -d "$WEB_ROOT" && "$WEB_ROOT" != "$WEB_ROOT_OLD" ]]; then
      mv "$WEB_ROOT" "$WEB_ROOT_OLD" 2>/dev/null || true
    fi
    die "Apache 配置校验失败，已回滚到变更前状态。apache2ctl configtest 报错：
${WS_TEST}"
  fi
  ws_reload || die "${WS_NAME} 重载失败，检查 systemctl status ${WS_SERVICE}"
  ok "${WS_NAME} 已重载，新域名已可访问（HTTP）"
else

# 能力规则（PHP / 静态 / WordPress 伪静态）在 snippets/site-<域名>.conf 里。
# 站点已用片段结构时，改域名要把片段一起改名沿用，不能用通用模板覆盖，
# 否则用户自己加的伪静态、自定义 location 会被静默丢掉。
SNIPPET_OLD="/etc/nginx/snippets/site-${OLD_DOMAIN}.conf"
SNIPPET_NEW="/etc/nginx/snippets/site-${NEW_DOMAIN}.conf"
TYPE_OLD="/etc/nginx/site-types/${OLD_DOMAIN}"
TYPE_NEW="/etc/nginx/site-types/${NEW_DOMAIN}"
SNIPPET_COPIED=0

if [[ -f "$SNIPPET_OLD" ]]; then
  # 域名里的 . 在 sed 里是通配符，不转义会把 pool_com.sock 也改成 poolnew.com.sock
  OLD_DOMAIN_ESC="${OLD_DOMAIN//./\\.}"
  sed "s|${OLD_DOMAIN_ESC}|${NEW_DOMAIN}|g" "$SNIPPET_OLD" > "$SNIPPET_NEW"
  SNIPPET_COPIED=1
  info "沿用能力片段：$SNIPPET_NEW"

cat > "$NGINX_NEW" <<'NGINX'
server {
    listen 80;
    listen [::]:80;
    server_name __DOMAIN__ www.__DOMAIN__;

    root __ROOT__;

    client_max_body_size 64m;
    client_body_timeout 60s;

    access_log /var/log/nginx/__DOMAIN__.access.log;
    error_log  /var/log/nginx/__DOMAIN__.error.log warn;

    include __SNIPPET__;
}
NGINX

  sed -i \
    -e "s|__DOMAIN__|${NEW_DOMAIN}|g" \
    -e "s|__ROOT__|${WEB_ROOT}|g" \
    -e "s|__SNIPPET__|${SNIPPET_NEW}|g" \
    "$NGINX_NEW"
else

cat > "$NGINX_NEW" <<'NGINX'
server {
    listen 80;
    listen [::]:80;
    server_name __DOMAIN__ www.__DOMAIN__;

    root __ROOT__;
    index index.php index.html;

    client_max_body_size 64m;
    client_body_timeout 60s;

    access_log /var/log/nginx/__DOMAIN__.access.log;
    error_log  /var/log/nginx/__DOMAIN__.error.log warn;

    location / {
        try_files $uri $uri/ /index.php?$args;
    }

    location ~ \.php$ {
        try_files $fastcgi_script_name =404;
        include fastcgi_params;
        fastcgi_pass unix:__SOCK__;
        fastcgi_index index.php;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
        fastcgi_read_timeout 120;
        fastcgi_buffers 16 16k;
        fastcgi_buffer_size 32k;
    }

    location ~* \.(js|css|png|jpg|jpeg|gif|ico|svg|webp|avif|woff|woff2|ttf)$ {
        expires 30d;
        access_log off;
        try_files $uri =404;
    }

    location ~ /\.(?!well-known) {
        deny all;
    }

    location = /wp-config.php {
        deny all;
    }

    location = /readme.html {
        deny all;
    }
}
NGINX

sed -i \
  -e "s|__DOMAIN__|${NEW_DOMAIN}|g" \
  -e "s|__ROOT__|${WEB_ROOT}|g" \
  -e "s|__SOCK__|${PHP_SOCK}|g" \
  "$NGINX_NEW"

fi

# 站点能力标记也跟着改名，否则新域名只能靠文件特征猜类型
if [[ -f "$TYPE_OLD" ]]; then
  cp -a "$TYPE_OLD" "$TYPE_NEW"
fi

# 记录旧域名当前启用的是哪个软链，失败时用于回滚
OLD_LINK=""
for _l in /etc/nginx/sites-enabled/*; do
  [[ -e "$_l" ]] || continue
  if [[ "$(readlink -f "$_l")" == "$NGINX_OLD" ]]; then
    OLD_LINK="$_l"
  fi
done

# 先只启用新域名，旧域名配置暂时保留在 sites-available，稍后转 301
ln -sf "$NGINX_NEW" "/etc/nginx/sites-enabled/${NEW_DOMAIN}"
if [[ -n "$OLD_LINK" ]]; then
  rm -f "$OLD_LINK"
else
  rm -f "/etc/nginx/sites-enabled/${OLD_DOMAIN}"
fi

if ! NGINX_TEST=$(nginx -t 2>&1); then
  # 回滚：撤掉新配置，恢复旧软链，保证站点不中断
  rm -f "/etc/nginx/sites-enabled/${NEW_DOMAIN}" "$NGINX_NEW"
  [[ "$SNIPPET_COPIED" == "1" ]] && rm -f "$SNIPPET_NEW"
  [[ -f "$TYPE_NEW" ]] && rm -f "$TYPE_NEW"
  if [[ -n "$OLD_LINK" && ! -e "$OLD_LINK" ]]; then
    ln -sf "$NGINX_OLD" "$OLD_LINK"
  else
    ln -sf "$NGINX_OLD" "/etc/nginx/sites-enabled/${OLD_DOMAIN}"
  fi
  systemctl reload nginx >/dev/null 2>&1 || true
  if [[ "$MOVE_DIR" == "1" && -d "$WEB_ROOT" && "$WEB_ROOT" != "$WEB_ROOT_OLD" ]]; then
    mv "$WEB_ROOT" "$WEB_ROOT_OLD" 2>/dev/null || true
  fi
  die "Nginx 配置校验失败，已回滚到变更前状态。nginx 报错：
${NGINX_TEST}"
fi
systemctl reload nginx || die "Nginx 重载失败，检查 systemctl status nginx"
ok "Nginx 已重载，新域名已可访问（HTTP）"

# 片段与类型标记已经复制到新域名下，旧的随旧域名一起退休，避免 snippets 目录堆残留
[[ "$SNIPPET_COPIED" == "1" && -f "$SNIPPET_OLD" ]] && rm -f "$SNIPPET_OLD"
[[ -f "$TYPE_OLD" ]] && rm -f "$TYPE_OLD"

fi
fi

# ---------------------------------------------------------------- 证书

section "HTTPS 证书"

SCHEME="http"
if [[ "$ENABLE_SSL" == "1" ]]; then
  if [[ "$DRY_RUN" == "1" ]]; then
    info "将为 $NEW_DOMAIN 申请证书"
    SCHEME="https"
  elif command -v certbot >/dev/null 2>&1; then
    CERT_ARGS=(-d "$NEW_DOMAIN")
    if getent hosts "www.$NEW_DOMAIN" >/dev/null 2>&1; then
      CERT_ARGS+=(-d "www.$NEW_DOMAIN")
    fi
    if certbot "$WS_CERT_PLUGIN" "${CERT_ARGS[@]}" --non-interactive --agree-tos --redirect \
         --email "admin@${NEW_DOMAIN}" 2>/dev/null; then
      SCHEME="https"
      ok "证书申请成功，已启用 HTTPS"
    else
      warn "证书申请失败，站点先以 http 运行；稍后可执行："
      warn "  certbot ${WS_CERT_PLUGIN} -d ${NEW_DOMAIN}"
    fi
  else
    warn "certbot 未安装，跳过 HTTPS"
  fi
else
  warn "已跳过证书申请，站点以 http 访问"
fi

# ---------------------------------------------------------------- 旧域名 301

section "旧域名处理"

if [[ "$DRY_RUN" == "1" ]]; then
  if [[ "$KEEP_REDIRECT" == "1" ]]; then
    info "将把 $NGINX_OLD 改写为 301 → ${SCHEME}://${NEW_DOMAIN} 并重新启用"
  else
    info "将删除旧域名配置 $NGINX_OLD"
  fi
elif [[ "$KEEP_REDIRECT" == "1" ]]; then

if [[ "$WS" == "apache" ]]; then
  # 旧域名留一个纯跳转的 vhost。旧域名还有证书时一并写 443，避免
  # 浏览器先跳 https 再吃证书错误。
  {
    echo "# 由 WordPress 管理面板生成：旧域名 301 → ${SCHEME}://${NEW_DOMAIN}"
    echo "<VirtualHost *:80>"
    echo "    ServerName ${OLD_DOMAIN}"
    echo "    ServerAlias www.${OLD_DOMAIN}"
    echo "    Redirect permanent / ${SCHEME}://${NEW_DOMAIN}/"
    echo "    ErrorLog  ${WS_LOG_DIR}/${OLD_DOMAIN}.error.log"
    echo "    CustomLog ${WS_LOG_DIR}/${OLD_DOMAIN}.access.log combined"
    echo "</VirtualHost>"
    if [[ -f "/etc/letsencrypt/live/${OLD_DOMAIN}/fullchain.pem" ]]; then
      echo ""
      echo "<VirtualHost *:443>"
      echo "    ServerName ${OLD_DOMAIN}"
      echo "    ServerAlias www.${OLD_DOMAIN}"
      echo "    SSLEngine on"
      echo "    SSLCertificateFile /etc/letsencrypt/live/${OLD_DOMAIN}/fullchain.pem"
      echo "    SSLCertificateKeyFile /etc/letsencrypt/live/${OLD_DOMAIN}/privkey.pem"
      if [[ -f /etc/letsencrypt/options-ssl-apache.conf ]]; then
        echo "    Include /etc/letsencrypt/options-ssl-apache.conf"
      fi
      echo "    Redirect permanent / ${SCHEME}://${NEW_DOMAIN}/"
      echo "    ErrorLog  ${WS_LOG_DIR}/${OLD_DOMAIN}.error.log"
      echo "    CustomLog ${WS_LOG_DIR}/${OLD_DOMAIN}.access.log combined"
      echo "</VirtualHost>"
    fi
  } > "$NGINX_OLD"

  ln -sfn "$NGINX_OLD" "$(ws_link "$OLD_DOMAIN")"

  if ! WS_TEST=$(ws_test); then
    rm -f "$(ws_link "$OLD_DOMAIN")"
    warn "旧域名 301 配置校验失败，已撤销（新域名不受影响）。报错："
    echo "$WS_TEST" >&2
  else
    ws_reload || warn "${WS_NAME} 重载失败，301 可能未生效"
    ok "旧域名 301 → ${SCHEME}://${NEW_DOMAIN}"
  fi
else
  cat > "$NGINX_OLD" <<'REDIRECT'
server {
    listen 80;
    listen [::]:80;
    server_name __OLD__ www.__OLD__;

    return 301 __TARGET__$request_uri;
}
REDIRECT
  sed -i \
    -e "s|__OLD__|${OLD_DOMAIN}|g" \
    -e "s|__TARGET__|${SCHEME}://${NEW_DOMAIN}|g" \
    "$NGINX_OLD"

  ln -sf "$NGINX_OLD" "/etc/nginx/sites-enabled/${OLD_DOMAIN}"

  if ! NGINX_TEST=$(nginx -t 2>&1); then
    rm -f "/etc/nginx/sites-enabled/${OLD_DOMAIN}"
    warn "旧域名 301 配置校验失败，已撤销（新域名不受影响）。nginx 报错："
    echo "$NGINX_TEST" >&2
  else
    systemctl reload nginx || warn "Nginx 重载失败，301 可能未生效"
    ok "旧域名 301 → ${SCHEME}://${NEW_DOMAIN}"
  fi
fi
else
  ws_disable "$OLD_DOMAIN"
  rm -f "$NGINX_OLD"
  warn "旧域名配置已删除，不再响应"
fi

# ---------------------------------------------------------------- 数据库替换

section "数据库地址替换"

if [[ "$DRY_RUN" == "1" ]]; then
  info "将执行 search-replace：https://${OLD_DOMAIN} → ${SCHEME}://${NEW_DOMAIN}"
  info "将执行 search-replace：${OLD_DOMAIN} → ${NEW_DOMAIN}"
fi

if [[ "$DRY_RUN" != "1" ]]; then
  if ! wp_cli_ok; then
    die "wp-cli 不可用（$WP_BIN），无法安全替换序列化数据。数据库备份在 $BACKUP_DIR"
  fi

  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    search-replace "https://${OLD_DOMAIN}" "${SCHEME}://${NEW_DOMAIN}" \
    --all-tables --precise --recurse-objects --skip-columns=guid \
    --path="$WEB_ROOT" >/dev/null
  ok "已替换 https://${OLD_DOMAIN}"

  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    search-replace "http://${OLD_DOMAIN}" "${SCHEME}://${NEW_DOMAIN}" \
    --all-tables --precise --recurse-objects --skip-columns=guid \
    --path="$WEB_ROOT" >/dev/null
  ok "已替换 http://${OLD_DOMAIN}"

  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    search-replace "${OLD_DOMAIN}" "${NEW_DOMAIN}" \
    --all-tables --precise --recurse-objects --skip-columns=guid \
    --path="$WEB_ROOT" >/dev/null
  ok "已替换正文中的裸域名"

  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    option update siteurl "${SCHEME}://${NEW_DOMAIN}" --path="$WEB_ROOT" >/dev/null
  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    option update home "${SCHEME}://${NEW_DOMAIN}" --path="$WEB_ROOT" >/dev/null
  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    cache flush --path="$WEB_ROOT" >/dev/null 2>&1 || true
  ok "siteurl / home 已更新为 ${SCHEME}://${NEW_DOMAIN}"
fi

# ---------------------------------------------------------------- 自检

section "自检"

if [[ "$DRY_RUN" == "1" ]]; then
  warn "DRY_RUN 模式，跳过自检与收尾"
  exit 0
fi

HTTP_CODE=$(curl -s -o /dev/null -m 15 -w '%{http_code}' -H "Host: ${NEW_DOMAIN}" http://127.0.0.1/) || HTTP_CODE="000"
case "$HTTP_CODE" in
  200|301|302) ok "新域名本地访问返回 ${HTTP_CODE}" ;;
  000)         warn "本地访问无响应，检查 ${WS_SERVICE} 与 php${PHP_VER}-fpm 状态" ;;
  *)           warn "本地访问返回 ${HTTP_CODE}，查看 ${WS_LOG_DIR}/${NEW_DOMAIN}.error.log" ;;
esac

CURRENT_HOME=$(runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
  option get home --path="$WEB_ROOT" 2>/dev/null || true)
if [[ "$CURRENT_HOME" == "${SCHEME}://${NEW_DOMAIN}" ]]; then
  ok "数据库 home 校验通过：$CURRENT_HOME"
else
  warn "数据库 home 为 ${CURRENT_HOME:-读取失败}，与预期 ${SCHEME}://${NEW_DOMAIN} 不符，请人工检查"
fi

section "完成"
cat <<SUMMARY

  新地址：${SCHEME}://${NEW_DOMAIN}
  后台：  ${SCHEME}://${NEW_DOMAIN}/wp-admin/
  目录：  ${WEB_ROOT}
  备份：  ${BACKUP_DIR}
  日志：  ${LOG}

还需要你确认：
  1. 云服务器安全组是否放行 80/443
  2. 旧域名 ${OLD_DOMAIN} 的 DNS 是否仍指向本机，301 才会生效
  3. CDN、对象存储、邮件模板、第三方登录回调里若写着旧域名，需要同步修改
  4. 建议用浏览器抽查首页、文章页、图片和后台各一处

回滚：
  配置：cp ${BACKUP_DIR}/site-${OLD_DOMAIN}.conf.bak ${NGINX_OLD}
  数据库：$DB_DUMP 库名 < ${BACKUP_DIR}/database.sql
  目录：mv ${WEB_ROOT_NEW} ${WEB_ROOT_OLD}，并改回 pool 的 chdir

SUMMARY
