#!/usr/bin/env bash
# WPQU panel — 网站日志
# LOG_ACTION=list|tail|clear|rotate|delete
#
# 只处理 nginx 站点日志、php-fpm 站点错误日志、WordPress debug.log 这三类。
# 路径一律白名单校验：面板能清空日志，但不能变成任意文件写入/删除的入口。

set -uo pipefail
export LC_ALL=C

LOG_ACTION="${LOG_ACTION:-list}"
LOG_TARGET="${LOG_TARGET:-}"
LOG_LINES="${LOG_LINES:-200}"

LOG_FILE=/var/log/wp-maint.log
NGINX_SNIPPETS=/etc/nginx/snippets

step() { printf '\n▸ %s\n' "$*"; }
ok()   { printf '  ✓ %s\n' "$*"; }
warn() { printf '  ! %s\n' "$*"; }
die()  { printf '[err] %s\n' "$*" >&2; exit 1; }
b64()  { printf '%s' "$1" | base64 | tr -d '\n'; }

begin_log() {
  [ -d /var/log ] || return 0
  [ -f "$LOG_FILE" ] && [ "$(stat -c %s "$LOG_FILE" 2>/dev/null || echo 0)" -gt 2097152 ] && : > "$LOG_FILE"
  # tee 自己的 stderr 要吞掉，否则日志目录不可写时会污染面板输出
  exec > >(tee -a "$LOG_FILE" 2>/dev/null) 2>&1
}

[[ "$LOG_LINES" =~ ^[0-9]+$ ]] || LOG_LINES=200
[ "$LOG_LINES" -lt 1 ] && LOG_LINES=200
[ "$LOG_LINES" -gt 2000 ] && LOG_LINES=2000

# ---------------------------------------------------------------- 路径校验

validate_path() {
  local p="$1"
  case "$p" in
    *..*) die "路径里不能出现 .." ;;
  esac
  case "$p" in
    /var/log/nginx/*|/var/log/apache2/*|/var/log/php-fpm-*.log|/var/log/php-fpm-*.log.*|/var/www/*/wp-content/debug.log|/var/www/*/wp-content/debug.log.*) : ;;
    *) die "只允许操作 nginx / apache / php-fpm / WordPress 调试日志：${p}" ;;
  esac
  case "$p" in
    *[\`\$\;\&\|\<\>]*) die "路径里有非法字符" ;;
  esac
}

# ---------------------------------------------------------------- 列表

declare -A ROOT_DOMAIN=()
declare -A SEEN=()

# 当前在跑哪个 Web 服务器：日志目录与站点配置的路径都由它决定
WEB_SERVER=nginx
if systemctl is-active --quiet apache2 2>/dev/null && ! systemctl is-active --quiet nginx 2>/dev/null; then
  WEB_SERVER=apache
fi
WEB_LOG_DIR=/var/log/nginx
[ "$WEB_SERVER" = apache ] && WEB_LOG_DIR=/var/log/apache2

site_root() {
  awk '$1 == "root" { gsub(/;/, "", $2); print $2; exit }' "$1" 2>/dev/null
}

site_confs() {
  local f
  if [ "$WEB_SERVER" = apache ]; then
    for f in /etc/apache2/sites-enabled/*.conf; do
      [ -e "$f" ] || continue
      case "$(basename "$f")" in 000-default.conf|default.conf) continue ;; esac
      printf '%s\n' "$f"
    done
  else
    for f in /etc/nginx/sites-enabled/*; do
      [ -e "$f" ] || continue
      printf '%s\n' "$f"
    done
  fi
}

site_first_domain() {
  local f="$1"
  if [ "$WEB_SERVER" = apache ]; then
    awk '/^[ \t]*ServerName[ \t]/ { sub(/^[ \t]*ServerName[ \t]+/, ""); sub(/#.*/, ""); print; exit }' "$f" 2>/dev/null | tr -d '"'
  else
    awk '/server_name/ {for (i=2; i<=NF; i++) { gsub(/;/, "", $i); if ($i != ";" && $i != "") { print $i; exit } } }' "$f" 2>/dev/null
  fi
}

apache_log_path() {
  local file="$1" key="$2" p
  p=$(awk -v k="$key" '{ for (i=1;i<=NF;i++) if ($i == k) { print $(i+1); exit } }' "$file" 2>/dev/null)
  p=${p%\"}
  p=${p#\"}
  case "$p" in
    /*) printf '%s' "$p" ;;
    *)  printf '' ;;
  esac
}

log_path() {
  # 取 access_log / error_log 的路径，忽略 off / syslog: / 变量形式
  local file="$1" key="$2" p
  p=$(awk -v k="$key" '$1 == k { gsub(/;/, "", $2); print $2; exit }' "$file" 2>/dev/null)
  p=${p%\"}
  p=${p#\"}
  case "$p" in
    /*) printf '%s' "$p" ;;
    *)  printf '' ;;
  esac
}

emit() {
  local domain="$1" kind="$2" path="$3" archive="${4:-0}"
  [ -e "$path" ] || return 0
  [ -n "${SEEN[$path]:-}" ] && return 0
  SEEN[$path]=1
  printf 'PANEL_LOG=%s|%s|%s|%s|%s|%s\n' \
    "$(b64 "$domain")" "$kind" "$(b64 "$path")" \
    "$(stat -c %s "$path" 2>/dev/null || echo 0)" \
    "$(stat -c %Y "$path" 2>/dev/null || echo 0)" "$archive"
}

do_list() {
  local conf domain root al el f

  # 域名 ← 站点根目录，供 PHP / debug.log 反查
  while IFS= read -r conf; do
    domain=$(site_first_domain "$conf")
    [ -n "$domain" ] || continue
    root=$(site_root "$conf")
    if [ -z "$root" ]; then
      root=$(awk '/^[ \t]*DocumentRoot[ \t]/ { sub(/^[ \t]*DocumentRoot[ \t]+/, ""); sub(/#.*/, ""); print; exit }' "$conf" 2>/dev/null | tr -d '"')
    fi
    [ -n "$root" ] && ROOT_DOMAIN["$root"]="$domain"
  done < <(site_confs)

  # 1) 每个站点自己的 access / error
  while IFS= read -r conf; do
    domain=$(site_first_domain "$conf")
    [ -n "$domain" ] || continue
    if [ "$WEB_SERVER" = apache ]; then
      al=$(apache_log_path "$conf" CustomLog)
      el=$(apache_log_path "$conf" ErrorLog)
    else
      al=$(log_path "$conf" access_log)
      el=$(log_path "$conf" error_log)
    fi
    [ -n "$al" ] || al="${WEB_LOG_DIR}/access.log"
    [ -n "$el" ] || el="${WEB_LOG_DIR}/error.log"
    emit "$domain" access "$al"
    emit "$domain" error "$el"
  done < <(site_confs)

  # 2) 还没被站点引用的 nginx 日志（全局日志、旧域名日志）
  for f in "${WEB_LOG_DIR}"/*.log; do
    [ -f "$f" ] || continue
    emit "-" other "$f"
  done

  # 3) PHP-FPM 站点错误日志：从 pool 配置反查 chdir，再映射到域名
  local pool err chdir
  for pool in /etc/php/*/fpm/pool.d/*.conf; do
    [ -f "$pool" ] || continue
    err=$(awk -F'=[[:space:]]*' '/error_log/ {print $2; exit}' "$pool" 2>/dev/null | tr -d '[:space:]')
    case "$err" in
      /var/log/*) : ;;
      *) continue ;;
    esac
    chdir=$(awk -F'=[[:space:]]*' '/^[[:space:]]*chdir/ {print $2; exit}' "$pool" 2>/dev/null | tr -d '[:space:]')
    domain="${ROOT_DOMAIN[$chdir]:-}"
    emit "$domain" php "$err"
  done
  # 兜底：没有 pool 引用的 php-fpm 日志也算上
  for f in /var/log/php-fpm-*.log; do
    [ -f "$f" ] || continue
    emit "-" php "$f"
  done

  # 4) WordPress 调试日志
  local root2
  for root2 in /var/www/*/wp-content/debug.log; do
    [ -f "$root2" ] || continue
    emit "${ROOT_DOMAIN["$(dirname "$(dirname "$root2")")"]:-}" debug "$root2"
  done

  # 5) 归档：排序后取前 60 个。哈希顺序是不稳定的，按哈希截断会让某些归档
  #    时不时「消失」，用户会以为文件被删了
  local base arch n=0
  local -a bases
  mapfile -t bases < <(printf '%s\n' "${!SEEN[@]}" | sort)
  for base in "${bases[@]}"; do
    for arch in "$base".*.gz "$base".[0-9] "$base".[0-9].gz; do
      [ -f "$arch" ] || continue
      emit "-" archive "$arch" 1
      n=$((n + 1))
      if [ "$n" -ge 60 ]; then return 0; fi
    done
  done
}

# ---------------------------------------------------------------- 查看

do_tail() {
  [ -n "$LOG_TARGET" ] || die "缺少文件路径"
  validate_path "$LOG_TARGET"
  [ -f "$LOG_TARGET" ] || die "文件不存在：${LOG_TARGET}"

  printf 'PANEL_TAIL=%s|%s|%s\n' "$(b64 "$LOG_TARGET")" "$(stat -c %s "$LOG_TARGET" 2>/dev/null || echo 0)" \
    "$(stat -c %Y "$LOG_TARGET" 2>/dev/null || echo 0)"
  printf '%s\n' '---8<---'
  # 归档坏了 gzip 会非 0 退出，但已解出的部分仍有价值，别整段丢掉
  case "$LOG_TARGET" in
    *.gz) gzip -dc "$LOG_TARGET" 2>/dev/null | tail -n "$LOG_LINES" || true ;;
    *)    tail -n "$LOG_LINES" "$LOG_TARGET" 2>/dev/null || true ;;
  esac
}

# ---------------------------------------------------------------- 清空

do_clear() {
  [ -n "$LOG_TARGET" ] || die "缺少文件路径"
  validate_path "$LOG_TARGET"
  [ -f "$LOG_TARGET" ] || die "文件不存在：${LOG_TARGET}"
  case "$LOG_TARGET" in
    *.gz|*.[0-9]|*.[0-9].gz) die "归档文件不能清空，直接删除即可" ;;
  esac

  local before
  before=$(stat -c %s "$LOG_TARGET" 2>/dev/null || echo 0)
  step "清空 ${LOG_TARGET}（${before} 字节）"
  # 用截断而非删除：nginx / php-fpm 持有的是同一个 inode，删掉会出现「空间没释放」
  : > "$LOG_TARGET"
  ok "已清空，文件句柄不受影响"
}

# ---------------------------------------------------------------- 轮转

do_rotate() {
  [ -n "$LOG_TARGET" ] || die "缺少文件路径"
  validate_path "$LOG_TARGET"
  [ -f "$LOG_TARGET" ] || die "文件不存在：${LOG_TARGET}"
  case "$LOG_TARGET" in
    *.gz|*.[0-9]|*.[0-9].gz) die "归档文件不能再轮转" ;;
  esac

  local sz stamp arch
  sz=$(stat -c %s "$LOG_TARGET" 2>/dev/null || echo 0)
  if [ "$sz" -eq 0 ]; then
    ok "文件是空的，不需要轮转"
    return 0
  fi

  stamp=$(date +%Y%m%d-%H%M%S)
  arch="${LOG_TARGET}.${stamp}.gz"
  step "轮转 $(basename "$LOG_TARGET")（${sz} 字节）"
  # 先复制再截断：写日志的进程一直持有原 inode，mv 走会让它继续写旧文件
  cp -p "$LOG_TARGET" "${LOG_TARGET}.${stamp}" || die "复制失败，磁盘可能满了"
  gzip -9 "${LOG_TARGET}.${stamp}" || die "压缩失败"
  : > "$LOG_TARGET"

  # 截断后让写日志的进程重新打开文件，否则它可能继续写已归档的那份
  case "$LOG_TARGET" in
    /var/log/nginx/*)
      nginx -s reopen >/dev/null 2>&1 || systemctl reload nginx >/dev/null 2>&1 || true
      ;;
    /var/log/apache2/*)
      systemctl reload apache2 >/dev/null 2>&1 || true
      ;;
  esac
  ok "已归档为 ${arch}"
}

# ---------------------------------------------------------------- 删除归档

do_delete() {
  [ -n "$LOG_TARGET" ] || die "缺少文件路径"
  validate_path "$LOG_TARGET"
  [ -f "$LOG_TARGET" ] || die "文件不存在：${LOG_TARGET}"
  case "$LOG_TARGET" in
    *.gz|*.[0-9]|*.[0-9].gz) : ;;
    *) die "只允许删除归档文件（.gz 或 .1 / .2 这类），当前文件请用「清空」" ;;
  esac
  rm -f "$LOG_TARGET" || die "删除失败"
  ok "已删除 ${LOG_TARGET}"
}

# ---------------------------------------------------------------- 入口

case "$LOG_ACTION" in
  list)   do_list ;;
  tail)   begin_log; do_tail ;;
  clear)  begin_log; do_clear ;;
  rotate) begin_log; do_rotate ;;
  delete) begin_log; do_delete ;;
  *)      die "未知操作：${LOG_ACTION}" ;;
esac
