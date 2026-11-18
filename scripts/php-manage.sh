#!/usr/bin/env bash
# WPQU panel — PHP 版本维护
# 由面板以 root 上传后执行：PHP_ACTION=status bash php-manage.sh
#
# 核心约束：
#   1. 站点用的是「按站点独立 pool」，socket 形如 /run/php/php8.2-fpm-wordpress.sock
#      pool 配置在 /etc/php/<版本>/fpm/pool.d/<名字>.conf
#      换版本 = 把 pool 配置搬到新版本目录 + 改 nginx 里指向的 socket + 重载
#   2. 卸载版本前必须确认没有站点在用它，否则整站 502
#   3. 任何改动都要先备份，失败回滚，不能让配置校验不过
#   4. Nginx 与 Apache 都支持：站点与 socket 的解析走 ws-lib 适配层

set -uo pipefail
# >>> mirror-lib BEGIN
# ---------------------------------------------------------------- 网络环境自适应（国内 / 海外）
#
# 面板跑在你自己电脑上，脚本跑在被管理的服务器上，所以「服务器在国内还是海外」
# 只能在服务器上判断。判断方式：对几组地址各发一次小请求，谁先回来就算谁的地盘。
#   国内：mirrors.aliyun.com、mirrors.cloud.tencent.com
#   海外：www.google.com、raw.githubusercontent.com
# 只有一边通 → 就是那边；两边都通 → 比平均耗时；都不通 → 不改任何东西。
#
# 结果缓存 12 小时到 /var/lib/wpqu/net.env。想强制指定：
#   echo 'REGION=global' > /etc/wpqu/net.conf      # cn / global / off
# off 表示完全不碰系统软件源，一切按官方地址走。
#
# 本段是从 _mirrors.sh 注入的副本（脚本要能单文件独立运行，所以不做 source）。
# 要改逻辑：改 _mirrors.sh，再执行
#     bash scripts/sync-mirrors.sh
# 直接改下面这段，下次注入就会被覆盖。

MIRROR_STATE=/var/lib/wpqu/net.env
MIRROR_CONF=/etc/wpqu/net.conf
MIRROR_TTL=43200

MIRROR_REGION=unknown
MIRROR_SUMMARY="未识别（按官方地址）"
MIRROR_APT_SWITCH=0
MIRROR_APT_CHANGED=0
MIRROR_APT_ERROR=""
# 云厂商的内网镜像：只有在那家云的机器上才解析得到，命中后比公网镜像快一个量级
# （实测腾讯云机器：mirrors.tencentyun.com 3.8ms，mirrors.aliyun.com 3s 才出首字节）
MIRROR_INTRANET=""
MIRROR_APT_HOST=mirrors.aliyun.com
MIRROR_PYPI=https://pypi.org/simple/
MIRROR_NPM=https://registry.npmjs.org
MIRROR_GOPROXY=https://proxy.golang.org,direct
MIRROR_GH=""
MIRROR_GH_FALLBACK=""
MIRROR_GO_DL=https://go.dev/dl/
MIRROR_NODE_DIST=https://nodejs.org/dist/
MIRROR_WP=https://wordpress.org/
MIRROR_DOCKER_REPO=https://download.docker.com

# 探一个地址，输出「状态码 耗时」；连不上给 000（耗时给 9，避免参与比较）
mirror_probe() { # $1=url $2=额外 curl 参数（可空）
  local r
  # shellcheck disable=SC2086
  r="$(curl -s ${2:-} -o /dev/null --max-time 2 -w '%{http_code} %{time_total}' "$1" 2>/dev/null || true)"
  case "$r" in
    [1-9][0-9][0-9]" "[0-9]*) printf '%s' "$r" ;;
    *) printf '000 9' ;;
  esac
}

# 并行探四组地址，汇总成 cn / global / unknown
mirror_detect() {
  local d cn_ok=0 cn_sum=0 gl_ok=0 gl_sum=0 code t f
  d="$(mktemp -d 2>/dev/null)" || {
    printf 'unknown'
    return 0
  }
  (mirror_probe https://mirrors.aliyun.com/ >"$d/cn1" 2>/dev/null) &
  (mirror_probe https://mirrors.cloud.tencent.com/ >"$d/cn2" 2>/dev/null) &
  (mirror_probe https://www.google.com/generate_204 >"$d/en1" 2>/dev/null) &
  (mirror_probe https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar "-r 0-2047" >"$d/en2" 2>/dev/null) &
  wait

  for f in cn1 cn2; do
    code=000
    t=9
    read -r code t <"$d/$f" 2>/dev/null || true
    if [ "${code:-000}" != "000" ]; then
      cn_ok=$((cn_ok + 1))
      cn_sum="$(awk -v a="$cn_sum" -v b="${t:-9}" 'BEGIN{printf "%.3f", a+b}')"
    fi
  done
  for f in en1 en2; do
    code=000
    t=9
    read -r code t <"$d/$f" 2>/dev/null || true
    if [ "${code:-000}" != "000" ]; then
      gl_ok=$((gl_ok + 1))
      gl_sum="$(awk -v a="$gl_sum" -v b="${t:-9}" 'BEGIN{printf "%.3f", a+b}')"
    fi
  done
  rm -rf "$d" 2>/dev/null || true

  if [ "$cn_ok" -gt 0 ] && [ "$gl_ok" -eq 0 ]; then
    printf 'cn'
  elif [ "$gl_ok" -gt 0 ] && [ "$cn_ok" -eq 0 ]; then
    printf 'global'
  elif [ "$cn_ok" -gt 0 ] && [ "$gl_ok" -gt 0 ]; then
    awk -v cok="$cn_ok" -v csum="$cn_sum" -v gok="$gl_ok" -v gsum="$gl_sum" \
      'BEGIN { c = csum/cok; g = gsum/gok; print (g < c*0.8) ? "global" : "cn" }'
  else
    printf 'unknown'
  fi
}

# 找云厂商的内网镜像。只有装在那家云的机器上才通，通就说明走内网，
# 又快又不算公网流量。判不出来返回空，退回公网镜像。
mirror_detect_intranet() {
  local u
  for u in mirrors.tencentyun.com mirrors.cloud.aliyuncs.com; do
    # 内网地址要么几毫秒回应，要么根本解析不到，1 秒足够
    if [ "$(curl -s -o /dev/null -m 1 -w '%{http_code}' "http://${u}/" 2>/dev/null || echo 000)" != "000" ]; then
      printf '%s' "$u"
      return 0
    fi
  done
  return 0
}

# 按区域把各项源的默认值填好；脚本可以再按需覆盖单项
mirror_apply_vars() {
  case "$MIRROR_REGION" in
    cn)
      MIRROR_APT_SWITCH=1
      MIRROR_NPM=https://registry.npmmirror.com
      MIRROR_GOPROXY=https://goproxy.cn,direct
      MIRROR_GH=https://ghfast.top/
      MIRROR_GH_FALLBACK=https://gh-proxy.com/
      MIRROR_GO_DL=https://golang.google.cn/dl/
      MIRROR_NODE_DIST=https://npmmirror.com/mirrors/node/
      MIRROR_WP=https://cn.wordpress.org/
      # 国内直连 download.docker.com 经常被重置，改从镜像的 docker-ce 仓库装，
      # 顺带省掉官方安装脚本那一跳（它要先去 get.docker.com 再拉几十 MB）。
      if [ -n "${MIRROR_INTRANET:-}" ]; then
        MIRROR_SUMMARY="国内（${MIRROR_INTRANET} 内网镜像）"
        MIRROR_APT_HOST="$MIRROR_INTRANET"
        MIRROR_PYPI="http://${MIRROR_INTRANET}/pypi/simple/"
        MIRROR_DOCKER_REPO="https://${MIRROR_INTRANET}/docker-ce"
      else
        MIRROR_SUMMARY="国内（阿里云镜像）"
        MIRROR_APT_HOST=mirrors.aliyun.com
        MIRROR_PYPI=https://mirrors.aliyun.com/pypi/simple/
        MIRROR_DOCKER_REPO=https://mirrors.aliyun.com/docker-ce
      fi
      ;;
    global)
      MIRROR_SUMMARY="海外（官方源）"
      MIRROR_APT_SWITCH=1
      MIRROR_PYPI=https://pypi.org/simple/
      MIRROR_NPM=https://registry.npmjs.org
      MIRROR_GOPROXY=https://proxy.golang.org,direct
      MIRROR_GH=""
      MIRROR_GH_FALLBACK=""
      MIRROR_GO_DL=https://go.dev/dl/
      MIRROR_NODE_DIST=https://nodejs.org/dist/
      MIRROR_WP=https://wordpress.org/
      MIRROR_DOCKER_REPO=https://download.docker.com
      ;;
    *)
      # 认不出来就别动系统：全部走官方地址，也不改 apt 源
      MIRROR_SUMMARY="未识别（按官方地址）"
      MIRROR_APT_SWITCH=0
      MIRROR_PYPI=https://pypi.org/simple/
      MIRROR_NPM=https://registry.npmjs.org
      MIRROR_GOPROXY=https://proxy.golang.org,direct
      MIRROR_GH=""
      MIRROR_GH_FALLBACK=""
      MIRROR_GO_DL=https://go.dev/dl/
      MIRROR_NODE_DIST=https://nodejs.org/dist/
      MIRROR_WP=https://wordpress.org/
      MIRROR_DOCKER_REPO=https://download.docker.com
      ;;
  esac
}

# 确定区域（优先级：WPQU_REGION 环境变量 > /etc/wpqu/net.conf > 缓存 > 现探）
mirror_init() {
  local want="" region="" ts=0
  want="${WPQU_REGION:-}"
  if [ -z "$want" ] && [ -r "$MIRROR_CONF" ]; then
    want="$(sed -n 's/^[[:space:]]*REGION[[:space:]]*=[[:space:]]*//p' "$MIRROR_CONF" 2>/dev/null | tail -1 | tr -d '"' | tr -d "'" | tr -d '[:space:]')"
  fi
  case "${want:-}" in
    off | OFF | none | no | 0) region="off" ;;
    cn | CN | china | domestic | 国内) region="cn" ;;
    global | GLOBAL | overseas | intl | international | 海外) region="global" ;;
  esac

  if [ -z "$region" ] && [ -r "$MIRROR_STATE" ]; then
    ts="$(awk -F= '$1 == "TS" {print $2; exit}' "$MIRROR_STATE" 2>/dev/null || true)"
    case "${ts:-}" in '' | *[!0-9]*) ts=0 ;; esac
    if [ "$ts" -gt 0 ] && [ $(($(date +%s) - ts)) -lt "$MIRROR_TTL" ]; then
      region="$(awk -F= '$1 == "REGION" {print $2; exit}' "$MIRROR_STATE" 2>/dev/null | tr -d '[:space:]' || true)"
      case "$region" in cn | global) ;; *) region="" ;; esac
      if [ "$region" = "cn" ]; then
        MIRROR_INTRANET="$(awk -F= '$1 == "INTRANET" {print $2; exit}' "$MIRROR_STATE" 2>/dev/null | tr -d '[:space:]' || true)"
      fi
    fi
  fi

  if [ -z "$region" ]; then
    if command -v curl >/dev/null 2>&1; then
      region="$(mirror_detect)"
    else
      region="unknown"
    fi
  fi
  case "${region:-}" in
    cn | global | off) MIRROR_REGION="$region" ;;
    *) MIRROR_REGION="unknown" ;;
  esac

  # 国内再补一次内网镜像判定（腾讯云 / 阿里云的内网源，命中就是几毫秒）
  if [ "$MIRROR_REGION" = "cn" ] && [ -z "${MIRROR_INTRANET:-}" ] && command -v curl >/dev/null 2>&1; then
    MIRROR_INTRANET="$(mirror_detect_intranet)"
  fi

  if [ "$MIRROR_REGION" = "cn" ] || [ "$MIRROR_REGION" = "global" ]; then
    mkdir -p "$(dirname "$MIRROR_STATE")" 2>/dev/null || true
    if [ "$MIRROR_REGION" = "cn" ] && [ -n "${MIRROR_INTRANET:-}" ]; then
      printf 'TS=%s\nREGION=%s\nINTRANET=%s\n' "$(date +%s)" "$MIRROR_REGION" "$MIRROR_INTRANET" \
        >"$MIRROR_STATE" 2>/dev/null || true
    else
      printf 'TS=%s\nREGION=%s\n' "$(date +%s)" "$MIRROR_REGION" >"$MIRROR_STATE" 2>/dev/null || true
    fi
  fi
  mirror_apply_vars
  return 0
}

# 给 GitHub 地址套加速前缀；没有前缀（海外）就原样返回
mirror_gh_url() {
  case "${1:-}" in
    *github.com/* | *githubusercontent.com/*)
      if [ -n "${MIRROR_GH:-}" ]; then
        printf '%s%s' "$MIRROR_GH" "$1"
      else
        printf '%s' "$1"
      fi
      ;;
    *) printf '%s' "${1:-}" ;;
  esac
}

# apt 索引更新：关掉翻译文件、限时重试，慢网络下省掉大半下载量
mirror_apt_update() {
  DEBIAN_FRONTEND=noninteractive apt-get update -qq \
    -o Acquire::Retries="${1:-1}" \
    -o Acquire::http::Timeout=15 \
    -o Acquire::https::Timeout=15 \
    -o Acquire::Languages=none
}

# 按区域输出 WordPress 安装包地址（每行一个，脚本自己读进数组）
mirror_wp_sources() { # $1=zh_CN | en_US
  if [ "${1:-}" = "en_US" ]; then
    if [ "$MIRROR_REGION" = "cn" ]; then
      printf '%s\n%s\n' \
        "https://cn.wordpress.org/latest.tar.gz" \
        "https://wordpress.org/latest.tar.gz"
    else
      printf '%s\n%s\n' \
        "https://wordpress.org/latest.tar.gz" \
        "https://cn.wordpress.org/latest.tar.gz"
    fi
    return 0
  fi
  # 实测：cn.wordpress.org 的 latest-zh_CN.tar.gz 常 404，wordpress-latest-zh_CN 才可用
  if [ "$MIRROR_REGION" = "cn" ]; then
    printf '%s\n%s\n%s\n' \
      "https://cn.wordpress.org/wordpress-latest-zh_CN.tar.gz" \
      "https://wordpress.org/latest-zh_CN.tar.gz" \
      "https://cn.wordpress.org/latest-zh_CN.tar.gz"
  else
    printf '%s\n%s\n%s\n' \
      "https://wordpress.org/latest-zh_CN.tar.gz" \
      "https://cn.wordpress.org/wordpress-latest-zh_CN.tar.gz" \
      "https://cn.wordpress.org/latest-zh_CN.tar.gz"
  fi
}

# 按区域输出 wp-cli.phar 的候选地址（每行一个）
mirror_wpcli_urls() {
  local phar="https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar"
  if [ "$MIRROR_REGION" = "cn" ]; then
    printf '%s\n%s\n%s\n%s\n' \
      "https://ghfast.top/${phar}" \
      "https://gh-proxy.com/${phar}" \
      "https://fastly.jsdelivr.net/gh/wp-cli/builds@gh-pages/phar/wp-cli.phar" \
      "$phar"
  else
    printf '%s\n%s\n%s\n' \
      "$phar" \
      "https://fastly.jsdelivr.net/gh/wp-cli/builds@gh-pages/phar/wp-cli.phar" \
      "https://cdn.jsdelivr.net/gh/wp-cli/builds@gh-pages/phar/wp-cli.phar"
  fi
}

# 装 uv。官方安装器要从 GitHub Releases 取二进制：
# 国内直连基本拉不动，先套加速前缀，一个前缀不行换下一个（实测 ghfast 对
# 大文件偶尔 403，gh-proxy 可以），都不行才退回直连。成功返回 0。
mirror_install_uv() {
  local dir="${1:-/usr/local/bin}" script p
  script="$(mktemp 2>/dev/null)" || return 1
  if ! curl -fsSL --max-time 60 https://astral.sh/uv/install.sh -o "$script" 2>/dev/null; then
    rm -f "$script" 2>/dev/null || true
    return 1
  fi
  if [ -n "${MIRROR_GH:-}" ]; then
    for p in "$MIRROR_GH" "${MIRROR_GH_FALLBACK:-}" ""; do
      if UV_INSTALLER_GITHUB_BASE_URL="${p}https://github.com" UV_INSTALL_DIR="$dir" \
        sh "$script" >/dev/null 2>&1; then
        rm -f "$script" 2>/dev/null || true
        return 0
      fi
    done
  elif UV_INSTALL_DIR="$dir" sh "$script" >/dev/null 2>&1; then
    rm -f "$script" 2>/dev/null || true
    return 0
  fi
  rm -f "$script" 2>/dev/null || true
  return 1
}

# 只改源地址里的主机段（保留套件、组件、签名配置），并做一次可用性验证
mirror_apt_rewrite() {
  local f="$1" h="${MIRROR_APT_HOST:-mirrors.aliyun.com}"
  if [ "$MIRROR_REGION" = "cn" ]; then
    sed -i \
      -e "s#deb\.debian\.org/debian-security#${h}/debian-security#g" \
      -e "s#security\.debian\.org/debian-security#${h}/debian-security#g" \
      -e "s#security\.debian\.org#${h}/debian-security#g" \
      -e "s#deb\.debian\.org/debian#${h}/debian#g" \
      -e "s#archive\.ubuntu\.com/ubuntu#${h}/ubuntu#g" \
      -e "s#security\.ubuntu\.com/ubuntu#${h}/ubuntu#g" \
      -e "s#ports\.ubuntu\.com/ubuntu-ports#${h}/ubuntu-ports#g" \
      -e 's#download\.docker\.com/linux/debian#mirrors.aliyun.com/docker-ce/linux/debian#g' \
      -e 's#download\.docker\.com/linux/ubuntu#mirrors.aliyun.com/docker-ce/linux/ubuntu#g' \
      "$f"
  elif [ "$MIRROR_REGION" = "global" ]; then
    sed -i \
      -e 's#mirrors\.aliyun\.com/debian-security#deb.debian.org/debian-security#g' \
      -e 's#mirrors\.cloud\.tencent\.com/debian-security#deb.debian.org/debian-security#g' \
      -e 's#mirrors\.tencentyun\.com/debian-security#deb.debian.org/debian-security#g' \
      -e 's#mirrors\.cloud\.aliyuncs\.com/debian-security#deb.debian.org/debian-security#g' \
      -e 's#mirrors\.tuna\.tsinghua\.edu\.cn/debian-security#deb.debian.org/debian-security#g' \
      -e 's#mirrors\.aliyun\.com/debian#deb.debian.org/debian#g' \
      -e 's#mirrors\.cloud\.tencent\.com/debian#deb.debian.org/debian#g' \
      -e 's#mirrors\.tencentyun\.com/debian#deb.debian.org/debian#g' \
      -e 's#mirrors\.cloud\.aliyuncs\.com/debian#deb.debian.org/debian#g' \
      -e 's#mirrors\.tuna\.tsinghua\.edu\.cn/debian#deb.debian.org/debian#g' \
      -e 's#mirrors\.ustc\.edu\.cn/debian#deb.debian.org/debian#g' \
      -e 's#mirrors\.huaweicloud\.com/debian#deb.debian.org/debian#g' \
      -e 's#mirrors\.aliyun\.com/ubuntu-ports#ports.ubuntu.com/ubuntu-ports#g' \
      -e 's#mirrors\.cloud\.tencent\.com/ubuntu-ports#ports.ubuntu.com/ubuntu-ports#g' \
      -e 's#mirrors\.aliyun\.com/ubuntu#archive.ubuntu.com/ubuntu#g' \
      -e 's#mirrors\.cloud\.tencent\.com/ubuntu#archive.ubuntu.com/ubuntu#g' \
      -e 's#mirrors\.tencentyun\.com/ubuntu#archive.ubuntu.com/ubuntu#g' \
      -e 's#mirrors\.tuna\.tsinghua\.edu\.cn/ubuntu#archive.ubuntu.com/ubuntu#g' \
      -e 's#mirrors\.ustc\.edu\.cn/ubuntu#archive.ubuntu.com/ubuntu#g' \
      -e 's#mirrors\.huaweicloud\.com/ubuntu#archive.ubuntu.com/ubuntu#g' \
      -e 's#mirrors\.aliyun\.com/docker-ce/linux/debian#download.docker.com/linux/debian#g' \
      -e 's#mirrors\.aliyun\.com/docker-ce/linux/ubuntu#download.docker.com/linux/ubuntu#g' \
      "$f"
  fi
  return 0
}

# 按区域把系统 apt 源换成就近镜像；换完验证不通就原样撤回。
# 备份文件叫 xxx.wpqu.bak，只留一份，永远是你机器原本的内容。
mirror_apt_switch() {
  [ "${MIRROR_APT_SWITCH:-0}" = "1" ] || return 0
  command -v apt-get >/dev/null 2>&1 || return 0
  local files f tmp changed=""
  files=""
  [ -f /etc/apt/sources.list ] && files="/etc/apt/sources.list"
  for f in /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
    [ -f "$f" ] || continue
    case "$f" in *.wpqu.bak | *.wpqu.tmp) continue ;; esac
    files="$files $f"
  done
  [ -n "$files" ] || return 0

  for f in $files; do
    grep -qE 'deb |URIs:' "$f" 2>/dev/null || continue
    tmp="$f.wpqu.tmp"
    cp -a "$f" "$tmp" 2>/dev/null || continue
    mirror_apt_rewrite "$tmp" || {
      rm -f "$tmp" 2>/dev/null || true
      continue
    }
    if cmp -s "$f" "$tmp" 2>/dev/null; then
      rm -f "$tmp" 2>/dev/null || true
      continue
    fi
    [ -f "$f.wpqu.bak" ] || cp -a "$f" "$f.wpqu.bak" 2>/dev/null || true
    cat "$tmp" >"$f"
    rm -f "$tmp" 2>/dev/null || true
    changed="$changed $f"
  done
  [ -n "$changed" ] || return 0

  if mirror_apt_update 1 >/dev/null 2>&1; then
    MIRROR_APT_CHANGED=1
    return 0
  fi
  for f in $changed; do
    if [ -f "$f.wpqu.bak" ]; then
      cp -a "$f.wpqu.bak" "$f" 2>/dev/null || true
    fi
  done
  MIRROR_APT_ERROR="换源后 apt 索引更新失败，已恢复原来的软件源"
  return 1
}
# <<< mirror-lib END
export LC_ALL=C
export DEBIAN_FRONTEND=noninteractive

PHP_ACTION="${PHP_ACTION:-status}"
PHP_TARGET="${PHP_TARGET:-}"
PHP_DOMAIN="${PHP_DOMAIN:-}"
PHP_FORCE="${PHP_FORCE:-0}"

LOG_FILE=/var/log/wp-maint.log
NGINX_SNIPPETS=/etc/nginx/snippets
POOL_ROOT=/etc/php

# 没装 Web 服务器时（纯 PHP 环境）不报错，只是站点列表为空。
# ws_init 定义在下面的适配层里，必须等注入块加载完再调用（见 ws-lib END 之后），
# 在这里调用只会 command not found，WS_ENABLED 为空 —— 站点列表和「卸载前检查站点占用」都会失效。
WS_OK=0

step() { printf '\n▸ %s\n' "$*"; }
ok()   { printf '  ✓ %s\n' "$*"; }
warn() { printf '  ! %s\n' "$*"; }
die()  { printf '[err] %s\n' "$*" >&2; exit 1; }
b64()  { printf '%s' "$1" | base64 | tr -d '\n'; }

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

# 适配层已就位，现在才能确定 Web 服务器；失败（纯 PHP 机器）时只是站点列表为空
if ws_init >/dev/null 2>&1; then WS_OK=1; fi


# 只读操作（status）不写日志，避免每刷一次页面就涨一屏
begin_log() {
  [ -d /var/log ] || return 0
  [ -f "$LOG_FILE" ] && [ "$(stat -c %s "$LOG_FILE" 2>/dev/null || echo 0)" -gt 2097152 ] && : > "$LOG_FILE"
  # tee 自己的 stderr 要吞掉，否则日志目录不可写时会污染面板输出
  exec > >(tee -a "$LOG_FILE" 2>/dev/null) 2>&1
}

# 装 PHP / 加软件源之前先定网络环境并换源：海外机器指着国内镜像、
# 国内机器指着 deb.debian.org，都能让一次安装从 1 分钟变成 10 分钟。
prepare_mirrors() {
  mirror_init
  printf '  网络环境：%s\n' "$MIRROR_SUMMARY"
  if ! mirror_apt_switch; then
    warn "$MIRROR_APT_ERROR"
  elif [[ "$MIRROR_APT_CHANGED" == "1" ]]; then
    printf '  软件源已切到就近镜像\n'
  fi
  return 0
}

# ---------------------------------------------------------------- 基础探测

os_field() { ( . /etc/os-release 2>/dev/null; eval "printf '%s' \"\${$1:-}\"" ); }

OS_ID=$(os_field ID)
[ -n "$OS_ID" ] || OS_ID=unknown
OS_VER=$(os_field VERSION_ID)
OS_CODENAME=$(os_field VERSION_CODENAME)
[ -n "$OS_CODENAME" ] || OS_CODENAME=$(os_field UBUNTU_CODENAME)

case "$OS_ID" in
  debian|ubuntu) : ;;
  *) die "只支持 Debian / Ubuntu，当前是 ${OS_ID}" ;;
esac

installed_versions() {
  local d v
  for d in "$POOL_ROOT"/*/fpm; do
    [ -d "$d" ] || continue
    v=$(basename "$(dirname "$d")")
    [[ "$v" =~ ^[0-9]+\.[0-9]+$ ]] && printf '%s\n' "$v"
  done | sort -V
}

ver_re='^[78]\.[0-9]$'

fpm_running() { systemctl is-active --quiet "php$1-fpm"; }

have_version() {
  local v
  for v in $(installed_versions); do
    [ "$v" = "$1" ] && return 0
  done
  return 1
}

# 上一次安装没结束时 apt 会锁住 dpkg，此时再发起一次只会得到一行看不懂的 E: 报错
check_apt_free() {
  local holder=""
  if command -v fuser >/dev/null 2>&1; then
    holder=$(fuser /var/lib/dpkg/lock-frontend 2>/dev/null | tr -d ' ')
  fi
  if [ -n "$holder" ]; then
    die "另一个 apt 进程正在运行（PID ${holder}），可能是上一次安装还没跑完。等它结束再试。"
  fi
}

# apt-cache policy 的 Candidate 行判断。注意不能写成 apt-cache ... | grep -q：
# 脚本开了 pipefail，grep 命中就退出会让 apt-cache 收到 SIGPIPE，整个管道判成失败
pkg_available() {
  local pol
  pol=$(apt-cache policy "$1" 2>/dev/null)
  case "$pol" in
    *"Candidate: (none)"*) return 1 ;;
    *"Candidate: "*) return 0 ;;
  esac
  return 1
}

cli_version() {
  php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null
}

ext_count() {
  local n
  n=$(php"$1" -m 2>/dev/null | grep -c '^[a-z]')
  printf '%s' "${n:-0}"
}

# 某个 socket 属于哪个 pool 文件（按 listen 精确匹配，避免 site_wordpress.conf / wordpress.conf 混淆）
find_pool_file() {
  local ver="$1" sock="$2" f
  for f in "$POOL_ROOT/$ver"/fpm/pool.d/*.conf; do
    [ -f "$f" ] || continue
    if grep -qE "^[[:space:]]*listen[[:space:]]*=[[:space:]]*${sock//./\\.}[[:space:]]*$" "$f"; then
      printf '%s' "$f"
      return 0
    fi
  done
  return 1
}

# 站点的 root / 类型 / socket 都由适配层解析（Nginx 看 root、fastcgi_pass，
# Apache 看 DocumentRoot、SetHandler "proxy:unix:..."）
site_root() { ws_root_of "$1"; }
detect_site_type() { ws_type_of "$1" "$2" "$3"; }
site_socket() { ws_socket_of "$2" "$NGINX_SNIPPETS/site-$1.conf"; }
site_domain() { ws_names "$1" | awk '{print $1}'; }

sock_version() {
  case "$1" in
    */php[0-9]*.[0-9]*-fpm-*.sock) printf '%s' "$1" | sed -E 's|.*/php([0-9]+\.[0-9]+)-fpm-.*|\1|' ;;
    *) printf '' ;;
  esac
}

sock_pool() {
  case "$1" in
    */php[0-9]*.[0-9]*-fpm-*.sock) printf '%s' "$1" | sed -E 's|.*/php[0-9]+\.[0-9]+-fpm-(.*)\.sock$|\1|' ;;
    *) printf '' ;;
  esac
}

# ---------------------------------------------------------------- 状态

do_status() {
  local v sites_out

  printf 'PANEL_OS=%s|%s|%s\n' "$OS_ID" "${OS_VER:-}" "${OS_CODENAME:-}"
  printf 'PANEL_PKG=apt\n'

  local cli bin
  cli=$(cli_version)
  bin=$(readlink -f "$(command -v php 2>/dev/null)" 2>/dev/null)
  printf 'PANEL_CLI=%s\n' "$cli"
  printf 'PANEL_CLI_BIN=%s\n' "$bin"

  local vers
  vers=$(installed_versions)
  if [ -z "$vers" ]; then
    printf 'PANEL_NOPHP=1\n'
  fi

  for v in $vers; do
    printf 'PANEL_VER=%s|%s|%s\n' "$v" "$(fpm_running "$v" && echo 1 || echo 0)" "$(ext_count "$v")"
  done

  # 站点：域名 | 类型 | 版本 | pool | root | 是否与当前 socket 一致
  local conf domain root type sock ver pool
  while IFS= read -r conf; do
    domain=$(site_domain "$conf")
    [ -n "$domain" ] || continue
    root=$(site_root "$conf")
    [ -n "$root" ] || root="/var/www/$domain"
    type=$(detect_site_type "$domain" "$conf" "$root")
    sock=$(site_socket "$domain" "$conf")
    ver=$(sock_version "$sock")
    pool=$(sock_pool "$sock")
    printf 'PANEL_SITE=%s|%s|%s|%s|%s\n' "$(b64 "$domain")" "$type" "${ver:--}" "${pool:--}" "$(b64 "$root")"
  done < <(ws_list_confs)

  # 候选版本：当前源里能装 / 已装
  for v in 7.4 8.0 8.1 8.2 8.3 8.4 8.5; do
    if have_version "$v"; then
      printf 'PANEL_AVAIL=%s|installed|1\n' "$v"
      continue
    fi
    if pkg_available "php$v-fpm"; then
      printf 'PANEL_AVAIL=%s|ready|1\n' "$v"
    else
      printf 'PANEL_AVAIL=%s|missing|0\n' "$v"
    fi
  done

  # 软件源现状
  if [ "$OS_ID" = "ubuntu" ]; then
    if grep -rqsE 'ondrej/php|ppa.launchpadcontent.net/ondrej' /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null; then
      printf 'PANEL_EXTRA_REPO=1\n'
    else
      printf 'PANEL_EXTRA_REPO=0\n'
    fi
  else
    if grep -rqs 'packages.sury.org/php' /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null; then
      printf 'PANEL_EXTRA_REPO=1\n'
    else
      printf 'PANEL_EXTRA_REPO=0\n'
    fi
  fi
}

# ---------------------------------------------------------------- 软件源

add_repo() {
  step "添加 PHP 软件源"
  local v
  if [ "$OS_ID" = "ubuntu" ]; then
    command -v add-apt-repository >/dev/null 2>&1 || {
      mirror_apt_update 2 || true
      apt-get install -y -qq --no-install-recommends software-properties-common || die "安装 software-properties-common 失败"
    }
    add-apt-repository -y ppa:ondrej/php || die "添加 ondrej/php PPA 失败（检查网络或系统版本是否受支持）"
  else
    local key=/usr/share/keyrings/sury-php.gpg
    [ -s "$key" ] || {
      install -d -m 0755 /usr/share/keyrings
      step "下载 Sury 签名密钥"
      if command -v curl >/dev/null 2>&1; then
        curl -fsSL https://packages.sury.org/php/apt.gpg -o "$key" || die "下载 Sury 密钥失败（服务器可能访问不了 packages.sury.org）"
      elif command -v wget >/dev/null 2>&1; then
        wget -qO "$key" https://packages.sury.org/php/apt.gpg || die "下载 Sury 密钥失败（服务器可能访问不了 packages.sury.org）"
      else
        die "系统里既没有 curl 也没有 wget，先装一个再试"
      fi
      [ -s "$key" ] || die "Sury 密钥为空，下载被拦截了"
    }

    if [ -z "$OS_CODENAME" ]; then
      die "读不到系统代号（/etc/os-release 里没有 VERSION_CODENAME），无法配置 Sury 源"
    fi
    printf 'deb [signed-by=/usr/share/keyrings/sury-php.gpg] https://packages.sury.org/php/ %s main\n' "$OS_CODENAME" \
      > /etc/apt/sources.list.d/sury-php.list
    ok "已写入 /etc/apt/sources.list.d/sury-php.list（${OS_CODENAME}）"
  fi

  check_apt_free
  step "刷新软件包索引"
  mirror_apt_update 2 || die "apt-get update 失败，PHP 源可能不可用"
  ok "软件源已就绪"
  printf 'PANEL_EXTRA_REPO=1\n'
}

# ---------------------------------------------------------------- 安装

php_pkgs_for() {
  local v="$1" ext pkg out=""
  for ext in fpm cli mysql curl gd mbstring xml zip intl bcmath opcache readline sqlite3; do
    pkg="php${v}-${ext}"
    if pkg_available "$pkg"; then
      out="$out $pkg"
    fi
  done
  printf '%s' "${out# }"
}

do_install() {
  [[ "$PHP_TARGET" =~ $ver_re ]] || die "版本号不合法：${PHP_TARGET}（形如 8.3）"
  local v="$PHP_TARGET"

  if [ -d "$POOL_ROOT/$v/fpm" ]; then
    ok "PHP ${v} 已经装过了"
    do_status
    return 0
  fi

  local pkgs
  pkgs=$(php_pkgs_for "$v")
  [ -n "$pkgs" ] || die "当前软件源里没有 php${v}。先在「PHP 版本」页点「添加 PHP 软件源」，再装。"

  check_apt_free
  step "安装 PHP ${v}"
  warn "会安装 $(printf '%s\n' $pkgs | wc -w) 个包（fpm / cli / 常用扩展）"
  # shellcheck disable=SC2086
  apt-get install -y -qq --no-install-recommends \
    -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold \
    $pkgs || die "安装 PHP ${v} 失败。若提示 dpkg 被锁定，说明还有别的 apt 在跑，等它结束再试；否则看上面的 apt 报错。"

  [ -d "$POOL_ROOT/$v/fpm" ] || die "装完了但 /etc/php/${v}/fpm 不存在，安装没成功"

  systemctl enable "php${v}-fpm" >/dev/null 2>&1 || true
  systemctl start "php${v}-fpm" || die "php${v}-fpm 启动失败，用 journalctl -u php${v}-fpm 看原因"

  if ! "php-fpm${v}" -t >/dev/null 2>&1; then
    warn "php-fpm${v} -t 未通过，可能是既有 pool 配置有问题"
  fi

  ok "PHP ${v} 安装完成，FPM 已启动（尚未绑定任何站点）"
  do_status
}

# ---------------------------------------------------------------- 卸载

sites_using() {
  local want="$1" conf domain sock ver
  while IFS= read -r conf; do
    domain=$(site_domain "$conf")
    [ -n "$domain" ] || continue
    sock=$(site_socket "$domain" "$conf")
    ver=$(sock_version "$sock")
    [ "$ver" = "$want" ] && printf '%s ' "$domain"
  done < <(ws_list_confs)
  return 0
}

do_remove() {
  [[ "$PHP_TARGET" =~ $ver_re ]] || die "版本号不合法：${PHP_TARGET}"
  local v="$PHP_TARGET" users
  [ -d "$POOL_ROOT/$v" ] || die "PHP ${v} 没有安装"

  users=$(sites_using "$v")
  if [ -n "$users" ] && [ "$PHP_FORCE" != "1" ]; then
    die "还有站点在用 PHP ${v}：${users% }。先把这些站点切到别的版本，再来卸载。"
  fi

  local total=0
  for x in $(installed_versions); do total=$((total + 1)); done
  if [ "$total" -le 1 ] && [ "$PHP_FORCE" != "1" ]; then
    die "这是机器上最后一个 PHP 版本，卸载后所有 PHP 站点都会 502。确实要删请勾选强制。"
  fi

  check_apt_free
  step "卸载 PHP ${v}"
  local list
  list=$(dpkg-query -W -f='${Package} ${Status}\n' 2>/dev/null | awk -v p="php${v}-" '$2=="install" && $3=="ok" && $4=="installed" && index($1,p)==1 {print $1}')
  if [ -n "$list" ]; then
    # shellcheck disable=SC2086
    printf '%s\n' $list | head -40
    # shellcheck disable=SC2086
    apt-get purge -y -qq $list || die "卸载失败。若提示 dpkg 被锁定，说明还有别的 apt 在跑；否则看上面的 apt 报错。"
  else
    warn "dpkg 里没有 php${v}-* 包，直接清理目录"
  fi

  apt-get autoremove -y -qq >/dev/null 2>&1 || true
  rm -rf "$POOL_ROOT/$v"

  # CLI 默认指向被删版本时，切到还活着的最新版本
  local cur next
  cur=$(cli_version)
  if [ "$cur" = "$v" ]; then
    next=$(installed_versions | tail -1)
    if [ -n "$next" ]; then
      update-alternatives --set php "/usr/bin/php${next}" >/dev/null 2>&1 \
        || ln -sf "/usr/bin/php${next}" /usr/bin/php
      ok "CLI 默认版本已切到 ${next}"
    else
      warn "机器上已经没有 PHP 了，php 命令可能失效"
    fi
  fi

  ok "PHP ${v} 已卸载"
  do_status
}

# ---------------------------------------------------------------- 切换站点版本

do_site() {
  local domain="$PHP_DOMAIN"
  [[ "$PHP_TARGET" =~ $ver_re ]] || die "目标版本不合法：${PHP_TARGET}"
  [ -n "$domain" ] || die "缺少站点域名"

  local conf
  conf=$(ws_lookup "$domain" 2>/dev/null) || conf=""
  [ -n "$conf" ] && [ -f "$conf" ] || die "找不到站点 ${domain} 的 $(ws_name) 配置"

  local root type sock oldver
  root=$(site_root "$conf")
  [ -n "$root" ] || root="/var/www/$domain"
  type=$(detect_site_type "$domain" "$conf" "$root")
  if [ "$type" = "static" ]; then
    die "站点 ${domain} 是纯静态站，不跑 PHP，不需要切版本"
  fi

  sock=$(site_socket "$domain" "$conf")
  [ -n "$sock" ] || die "站点配置里找不到 PHP socket（Nginx 看 fastcgi_pass，Apache 看 SetHandler proxy:unix:），可能被手工改过，请检查 $conf"
  oldver=$(sock_version "$sock")
  [ -n "$oldver" ] || die "看不懂这个 socket：$sock"

  if [ "$oldver" = "$PHP_TARGET" ]; then
    ok "站点 ${domain} 已经是 PHP ${PHP_TARGET} 了"
    do_status
    return 0
  fi

  [ -d "$POOL_ROOT/$PHP_TARGET/fpm" ] || die "PHP ${PHP_TARGET} 还没安装，先安装再切换"
  systemctl is-active --quiet "php${PHP_TARGET}-fpm" || systemctl start "php${PHP_TARGET}-fpm" \
    || die "php${PHP_TARGET}-fpm 启动失败"

  local pool safe new_sock old_pool new_pool pool_name
  CREATED_POOL=0
  pool=$(sock_pool "$sock")
  [ -n "$pool" ] || die "socket 命名不是面板生成的格式（${sock}），不敢自动迁移"
  safe="$pool"
  new_sock="/run/php/php${PHP_TARGET}-fpm-${safe}.sock"

  step "迁移 PHP pool：${safe}"
  if old_pool=$(find_pool_file "$oldver" "$sock"); then
    ok "原 pool 配置：${old_pool}"
    pool_name=$(basename "$old_pool")
    new_pool="$POOL_ROOT/$PHP_TARGET/fpm/pool.d/${pool_name}"
    if [ -f "$new_pool" ]; then
      ok "新版本已有同名 pool：${new_pool}（直接复用）"
    else
      cp -a "$old_pool" "$new_pool"
      CREATED_POOL=1
      # 只改 listen 的 socket 路径，其余（chdir / 内存限制 / 错误日志）原样保留
      sed -i "s|${sock}|${new_sock}|g" "$new_pool"
      ok "已复制到 ${new_pool}"
    fi
  else
    new_pool="$POOL_ROOT/$PHP_TARGET/fpm/pool.d/${safe}.conf"
    warn "没找到 listen = ${sock} 的 pool 配置，可能站点连的是别的服务器的 FPM"
    warn "这种情况下只改 nginx 指向，需要你确认新版本上有对应的 socket"
    [ -S "$new_sock" ] || [ "$PHP_FORCE" = "1" ] || die "新 socket ${new_sock} 不存在，无法保证站点可用（要强行切换请勾选强制）"
  fi

  # 配置校验：新版本 pool 有问题会让整个版本的 FPM 起不来，先测
  if ! "php-fpm${PHP_TARGET}" -t >/dev/null 2>&1; then
    "php-fpm${PHP_TARGET}" -t 2>&1 | sed 's/^/    /'
    [ "$CREATED_POOL" = "1" ] && rm -f "$new_pool"
    die "php${PHP_TARGET}-fpm 配置校验未通过，已撤回 pool 改动"
  fi

  # 备份 + 改写配置里所有指向旧 socket 的地方（两种写法里都是同一串路径）
  local files=() f backup="" changed=0
  [ -f "$NGINX_SNIPPETS/site-$domain.conf" ] && files+=("$NGINX_SNIPPETS/site-$domain.conf")
  files+=("$conf")
  for f in "${files[@]}"; do
    grep -q "$sock" "$f" || continue
    backup="$f.wpqu-bak"
    cp -a "$f" "$backup"
    sed -i "s|${sock}|${new_sock}|g" "$f"
    changed=$((changed + 1))
    ok "已改写 $(basename "$f")"
  done
  [ "$changed" -gt 0 ] || die "没有任何文件指向 ${sock}，什么都没改"

  step "校验并重载"
  if ! WS_TEST=$(ws_test); then
    printf '%s\n' "$WS_TEST" | sed 's/^/    /'
    for f in "${files[@]}"; do
      [ -f "$f.wpqu-bak" ] && mv -f "$f.wpqu-bak" "$f"
    done
    [ "$CREATED_POOL" = "1" ] && rm -f "$new_pool"
    die "$(ws_name) 配置校验未通过，已回滚站点配置"
  fi

  systemctl reload "php${PHP_TARGET}-fpm" 2>/dev/null || systemctl restart "php${PHP_TARGET}-fpm" || true
  ws_reload || die "$(ws_name) 重载失败"

  # 等 socket 出来
  local i=0
  while [ "$i" -lt 20 ]; do
    [ -S "$new_sock" ] && break
    sleep 0.5
    i=$((i + 1))
  done
  if [ -S "$new_sock" ]; then
    ok "socket 已就绪：${new_sock}"
  else
    warn "socket ${new_sock} 还没出现，首个请求到达时 ondemand 才会拉起进程"
  fi

  # 清掉备份
  for f in "${files[@]}"; do rm -f "$f.wpqu-bak"; done

  ok "站点 ${domain} 已切到 PHP ${PHP_TARGET}（原 ${oldver}）"
  do_status
}

# ---------------------------------------------------------------- 默认 CLI

do_default() {
  [[ "$PHP_TARGET" =~ $ver_re ]] || die "版本号不合法：${PHP_TARGET}"
  [ -x "/usr/bin/php${PHP_TARGET}" ] || die "没有 /usr/bin/php${PHP_TARGET}"

  step "设置默认 CLI 为 PHP ${PHP_TARGET}"
  if update-alternatives --set php "/usr/bin/php${PHP_TARGET}" >/dev/null 2>&1; then
    ok "update-alternatives 已切换"
  else
    ln -sf "/usr/bin/php${PHP_TARGET}" /usr/bin/php
    ok "已把 /usr/bin/php 指向 ${PHP_TARGET}"
  fi
  php -v | head -1
  do_status
}

# ---------------------------------------------------------------- 入口

case "$PHP_ACTION" in
  status)  do_status ;;
  install) begin_log; prepare_mirrors; do_install ;;
  remove)  begin_log; do_remove ;;
  site)    begin_log; do_site ;;
  default) begin_log; do_default ;;
  repo)    begin_log; prepare_mirrors; add_repo ;;
  *)       die "未知操作：${PHP_ACTION}" ;;
esac
