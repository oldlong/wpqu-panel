#!/usr/bin/env bash
#
# 转换已有站点的能力类型：
#   wordpress  完整 WordPress 站（需要 PHP + 数据库，缺 WordPress 时自动安装）
#   custom     自助网站（启用 PHP + 保留数据库，不做框架专属 rewrite）
#   static     纯静态站（关闭 PHP，只提供静态文件；数据库保留不删）
#
# 只动站点的能力配置（Nginx 的能力片段 / Apache 的 VirtualHost）与必要的运行环境，
# 不删目录内容、不删数据库。当前用的是哪种 Web 服务器由脚本自动识别。
#
# 用法：
#   DOMAIN=blog.com TARGET_TYPE=static bash site-convert.sh
#
# 可选环境变量：
#   DOMAIN           站点主域名（必填）
#   TARGET_TYPE      wordpress / custom / static（必填）
#   ADMIN_USER       需要安装 WordPress 时的后台用户名，默认 admin
#   ADMIN_PASS       后台密码，留空自动生成
#   ADMIN_EMAIL      管理员邮箱，默认 admin@example.com
#   SITE_TITLE       站点标题，默认域名
#   WP_LOCALE        zh_CN / en_US，默认 zh_CN
#   DB_NAME / DB_USER / DB_PASS  新建数据库时的名字与密码，留空自动生成
#   PHP_MAX_CHILDREN 需要新建 pool 时的 worker 数，默认 4
#   SEPARATE_POOL    1 强制新建独立 pool / 0 优先复用现有 socket（默认）
#   PHP_MEMORY_LIMIT 默认 128M
#   DRY_RUN          1 只打印计划，不改动服务器
#   FORCE            1 当前类型与目标一致时也重新生成配置
#

set -Eeuo pipefail
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

if [[ $EUID -ne 0 ]]; then
  echo "请用 root 运行：sudo -i 后重新执行" >&2
  exit 1
fi

trap 'echo -e "\n[ERROR] 第 $LINENO 行命令失败（退出码 $?），已中止。日志：${LOG:-未创建}" >&2' ERR

LOG="/var/log/wp-site-convert-$(date +%Y%m%d-%H%M%S).log"
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

DOMAIN="${DOMAIN:-}"
TARGET_TYPE="${TARGET_TYPE:-}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASS="${ADMIN_PASS:-}"
ADMIN_EMAIL="${ADMIN_EMAIL:-}"
SITE_TITLE="${SITE_TITLE:-}"
WP_LOCALE="${WP_LOCALE:-zh_CN}"
DB_NAME="${DB_NAME:-}"
DB_USER="${DB_USER:-}"
DB_PASS="${DB_PASS:-}"
PHP_MAX_CHILDREN="${PHP_MAX_CHILDREN:-4}"
SEPARATE_POOL="${SEPARATE_POOL:-0}"
PHP_MEMORY_LIMIT="${PHP_MEMORY_LIMIT:-128M}"
DRY_RUN="${DRY_RUN:-0}"
FORCE="${FORCE:-0}"

[[ -n "$DOMAIN" ]] || die "必须提供 DOMAIN"
DOMAIN="${DOMAIN#http://}"; DOMAIN="${DOMAIN#https://}"; DOMAIN="${DOMAIN%%/*}"
case "$TARGET_TYPE" in
  wordpress|custom|static) ;;
  *) die "TARGET_TYPE 只能是 wordpress / custom / static，当前为：${TARGET_TYPE:-空}" ;;
esac

case "$TARGET_TYPE" in
  wordpress) TARGET_LABEL="WordPress" ;;
  custom)    TARGET_LABEL="自助网站（PHP + 数据库）" ;;
  static)    TARGET_LABEL="纯静态站点" ;;
esac

SAFE_NAME="${DOMAIN//[.-]/_}"
SAFE_NAME="${SAFE_NAME:0:12}"
SITE_TITLE="${SITE_TITLE:-$DOMAIN}"
ADMIN_EMAIL="${ADMIN_EMAIL:-admin@example.com}"
if ! [[ "$ADMIN_EMAIL" =~ ^[^@[:space:]]+@[^@[:space:]]+\.[A-Za-z]{2,}$ ]]; then
  ADMIN_EMAIL="admin@example.com"
fi

NEEDS_PHP=1
NEEDS_DB=1
if [[ "$TARGET_TYPE" == "static" ]]; then
  NEEDS_PHP=0
  NEEDS_DB=0
fi

if ! ws_init; then
  die "既没有运行中的 Nginx，也没有 Apache，无法定位站点配置"
fi
WS_NAME=$(ws_name)

WEB_ROOT="/var/www/${DOMAIN}"
NGINX_CONF=$(ws_conf "$DOMAIN")
ENABLED_LINK=$(ws_link "$DOMAIN")
SNIPPET_DIR="/etc/nginx/snippets"
SNIPPET_FILE="${SNIPPET_DIR}/site-${DOMAIN}.conf"
TYPE_FILE=$(ws_type_file "$DOMAIN")
TYPE_DIR=$(dirname "$TYPE_FILE")
INFO_FILE="/root/wp-${DOMAIN}-info.txt"

POOL_FILE=""
PHP_SOCK=""
PHP_VER=""
CREATED_POOL=0
CREATED_DB=0
CREATED_DB_NAME=""
BACKUP_TAR=""
CHANGED=0
HAD_SNIPPET=0
HAD_TYPE=0
[[ -f "$SNIPPET_FILE" ]] && HAD_SNIPPET=1
[[ -f "$TYPE_FILE" ]] && HAD_TYPE=1

rand_pass() {
  local raw
  if command -v openssl >/dev/null 2>&1; then
    raw=$(openssl rand -hex 32)
  else
    raw=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
  fi
  printf '%s' "${raw:0:20}"
}

# ---------------------------------------------------------------- 站点能力片段
#
# 与 add-site.sh 中的模板保持一致：切换类型只重写这一个文件。

write_snippet() {
  local file="$1" type="$2" sock="${3:-}"
  case "$type" in
    wordpress)
      cat > "$file" <<'SNIP'
# WordPress 站点规则（由管理面板生成，切换站点类型时会整块重写）
index index.php index.html;

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
SNIP
      sed -i "s|__SOCK__|${sock}|g" "$file"
      ;;
    custom)
      cat > "$file" <<'SNIP'
# 自助网站规则：启用 PHP，不做框架专属 rewrite（由使用者自行按需追加）
index index.php index.html;

location / {
    try_files $uri $uri/ =404;
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

# 不要把配置、备份、SQL 之类文件当静态资源发出去
location ~* \.(inc|sql|bak|log|sh|ini|conf|dist|orig|save|swp|old)$ {
    return 404;
}

location ~ /\.(?!well-known) {
    deny all;
}

location = /wp-config.php {
    deny all;
}
SNIP
      sed -i "s|__SOCK__|${sock}|g" "$file"
      ;;
    static)
      cat > "$file" <<'SNIP'
# 纯静态站点规则：不解析 PHP，也不连接 PHP-FPM
index index.html index.htm;

location / {
    try_files $uri $uri/ =404;
}

# 静态站不应该执行任何脚本；即便目录里混入了 .php 也直接 404
location ~ \.php$ {
    return 404;
}

location ~* \.(js|css|png|jpg|jpeg|gif|ico|svg|webp|avif|woff|woff2|ttf)$ {
    expires 30d;
    access_log off;
    try_files $uri =404;
}

# 从 WordPress 转成静态站时目录里可能留着 wp-config.php（含库密码），
# 这类文件绝不能当静态资源发出去
location ~* \.(inc|sql|bak|log|sh|ini|conf|dist|orig|save|swp|old)$ {
    return 404;
}

location = /wp-config.php {
    return 404;
}

location ~ /\.(?!well-known) {
    deny all;
}
SNIP
      ;;
    *)
      die "未知站点类型：$type"
      ;;
  esac
  chmod 644 "$file"
}

# ---------------------------------------------------------------- 占位页

is_placeholder() {
  [[ -f "$WEB_ROOT/index.html" ]] && grep -q 'wp-panel-placeholder' "$WEB_ROOT/index.html" 2>/dev/null
}

maybe_placeholder() {
  local kind="$1" lead steps rules_hint
  if [[ "$WS" == "apache" ]]; then
    rules_hint="<li>PHP 已启用；需要伪静态时在 <code>${WEB_ROOT}/.htaccess</code> 里追加</li>"
  else
    rules_hint="<li>PHP 已启用；需要伪静态时改 <code>${SNIPPET_FILE}</code></li>"
  fi
  if [[ -e "$WEB_ROOT/index.php" || -e "$WEB_ROOT/index.htm" ]]; then
    return 0
  fi
  if [[ -e "$WEB_ROOT/index.html" ]] && ! is_placeholder; then
    return 0
  fi

  if [[ "$kind" == "custom" ]]; then
    lead="站点目录与数据库已经就绪，把要运行的程序放进目录即可开始使用。"
    steps="<li>删除本页 <code>index.html</code>，把程序文件上传到 <code>${WEB_ROOT}</code></li>
      <li>数据库连接信息见 <code>${INFO_FILE}</code>：库名 <code>${DB_NAME}</code>、用户 <code>${DB_USER}</code></li>
      ${rules_hint}"
  else
    lead="这是一个纯静态站点，只提供静态文件访问，不解析 PHP。"
    steps="<li>把 HTML / CSS / JS / 图片上传到 <code>${WEB_ROOT}</code>，覆盖本页 <code>index.html</code> 即可</li>
      <li>本站不占用 PHP-FPM，内存开销几乎为零</li>
      <li>以后需要动态能力，可在面板「编辑站点」里切换为自助网站或 WordPress</li>"
  fi

  cat > "$WEB_ROOT/index.html" <<HTML
<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="generator" content="wp-panel-placeholder">
<title>${DOMAIN}</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 32px;
         font-family: -apple-system, "Segoe UI", "Noto Sans SC", system-ui, sans-serif;
         background: #f6f7f9; color: #1b1f24; }
  .card { width: min(560px, 100%); background: #fff; border: 1px solid #e6e8ec; border-radius: 14px;
          padding: 32px 30px; box-shadow: 0 1px 2px rgba(16,24,40,.04), 0 8px 24px rgba(16,24,40,.06); }
  h1 { margin: 0 0 6px; font-size: 22px; letter-spacing: -.01em; }
  .badge { display: inline-block; margin-bottom: 18px; padding: 3px 9px; border-radius: 999px;
           background: #eef2ff; color: #3b5bdb; font-size: 12px; font-weight: 500; }
  p { margin: 0 0 10px; color: #4a5058; font-size: 14px; line-height: 1.7; }
  code { background: #f2f4f7; padding: 1px 6px; border-radius: 5px; font-size: 12.5px;
         font-family: ui-monospace, "JetBrains Mono", Consolas, monospace; }
  ol { margin: 14px 0 0; padding-left: 20px; color: #4a5058; font-size: 13.5px; line-height: 1.9; }
  .foot { margin-top: 22px; padding-top: 14px; border-top: 1px solid #eef0f3; color: #8b9099; font-size: 12px; }
  @media (prefers-color-scheme: dark) {
    body { background: #131517; color: #e8eaed; }
    .card { background: #191c1f; border-color: #2a2f34; box-shadow: none; }
    p, ol { color: #a8afb8; }
    code { background: #22262a; }
    .badge { background: #1e2530; color: #93b4ff; }
    .foot { border-color: #262b30; color: #7c848d; }
  }
</style>
</head>
<body>
  <div class="card">
    <div class="badge">${TARGET_LABEL}</div>
    <h1>${DOMAIN}</h1>
    <p>${lead}</p>
    <ol>${steps}</ol>
    <div class="foot">目录：<code>${WEB_ROOT}</code> · 生成于 $(date '+%Y-%m-%d %H:%M')</div>
  </div>
</body>
</html>
HTML
  chown www-data:www-data "$WEB_ROOT/index.html" 2>/dev/null || true
  chmod 644 "$WEB_ROOT/index.html"
  ok "已更新占位页 index.html"
}

# ---------------------------------------------------------------- 前置检查

section "前置检查"

# 网络环境只影响「从哪儿下 WordPress / wp-cli」，这里不改系统软件源
mirror_init
info "网络环境：$MIRROR_SUMMARY"

[[ -f /etc/os-release ]] || die "无法识别系统版本"
. /etc/os-release
case "${ID:-}" in
  debian|ubuntu) ok "系统：${PRETTY_NAME}" ;;
  *) die "仅支持 Debian / Ubuntu" ;;
esac

systemctl is-active --quiet "$WS_SERVICE" || die "${WS_NAME} 未运行，先启动后再转换"
ok "${WS_NAME} 运行中"

[[ -e "$NGINX_CONF" ]] || die "找不到站点配置 $NGINX_CONF（站点 ${DOMAIN} 是否由本面板创建？）"

# 站点目录以现有配置里的 root 为准：老站点的目录不一定叫 /var/www/<域名>
# （例如 pc5000.com 的 root 是 /var/www/pc5000），按域名硬拼会误报「找不到目录」
CONF_ROOT=$(ws_root_of "$NGINX_CONF")
if [[ -n "$CONF_ROOT" ]]; then
  WEB_ROOT="${CONF_ROOT%/}"
elif [[ -d "/var/www/${DOMAIN}" ]]; then
  WEB_ROOT="/var/www/${DOMAIN}"
fi
[[ -n "$WEB_ROOT" && -d "$WEB_ROOT" ]] \
  || die "找不到站点目录（配置里 root 为「${CONF_ROOT:-空}」，默认路径 /var/www/${DOMAIN} 也不存在）"

# 当前类型：优先读面板写的标记文件，其次按文件特征推断
CURRENT_TYPE=$(ws_type_of "$DOMAIN" "$NGINX_CONF" "$WEB_ROOT")
case "$CURRENT_TYPE" in
  wordpress) CURRENT_LABEL="WordPress" ;;
  custom)    CURRENT_LABEL="自助网站" ;;
  *)         CURRENT_LABEL="纯静态站点" ;;
esac
ok "站点目录：$WEB_ROOT"
ok "当前类型：${CURRENT_TYPE}（${CURRENT_LABEL}）"

HAS_WP=0
if [[ -f "$WEB_ROOT/wp-settings.php" && -f "$WEB_ROOT/wp-config.php" ]]; then
  HAS_WP=1
fi

# SSL 现状：转换时要原样保留证书配置
HAS_SSL=0
if [[ "$WS" == "apache" ]]; then
  if grep -qs 'SSLCertificateFile' "$NGINX_CONF" 2>/dev/null \
     || [[ -f "/etc/letsencrypt/live/${DOMAIN}/fullchain.pem" ]]; then
    HAS_SSL=1
  fi
elif grep -q 'ssl_certificate ' "$NGINX_CONF" 2>/dev/null \
   && grep -q 'ssl_certificate_key ' "$NGINX_CONF" 2>/dev/null; then
  HAS_SSL=1
fi
SCHEME="http"
[[ "$HAS_SSL" == "1" ]] && SCHEME="https"

if [[ "$CURRENT_TYPE" == "$TARGET_TYPE" && "$FORCE" != "1" ]]; then
  ok "已经是 ${TARGET_LABEL}，无需转换。"
  echo "如果只是想重写配置，可加 FORCE=1 再执行一次。"
  exit 0
fi

if [[ "$NEEDS_PHP" == "1" ]]; then
  case "$PHP_MAX_CHILDREN" in ''|*[!0-9]*) die "PHP_MAX_CHILDREN 必须是数字" ;; esac
  [[ "$PHP_MAX_CHILDREN" -ge 1 && "$PHP_MAX_CHILDREN" -le 12 ]] || die "PHP_MAX_CHILDREN 建议在 1-12 之间"
  case "$SEPARATE_POOL" in 0|1) ;; *) die "SEPARATE_POOL 只能是 0 或 1" ;; esac

  V=""
  if command -v php >/dev/null 2>&1; then
    V=$(php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null || true)
  fi
  if [[ -z "$V" ]]; then
    V=$(ls -1d /etc/php/*/fpm 2>/dev/null | awk -F/ '{print $(NF-1)}' | sort -V | awk 'END{print}')
  fi
  [[ -n "$V" ]] || die "检测不到 PHP-FPM，无法切换到需要 PHP 的类型"
  PHP_VER="$V"
  ok "PHP：$PHP_VER"
fi

# ---------------------------------------------------------------- 计划

# 需要安装 WordPress 吗（目标为 WP、但目录里没有 WP）
NEED_INSTALL_WP=0
if [[ "$TARGET_TYPE" == "wordpress" && "$HAS_WP" == "0" ]]; then
  if [[ -e "$WEB_ROOT/index.php" || -e "$WEB_ROOT/index.htm" ]]; then
    die "目录 $WEB_ROOT 里已有 index.php / index.htm，自动安装 WordPress 会覆盖现有程序。
    请先备份并移走现有文件，或改用「自助网站」类型。"
  fi
  if [[ -e "$WEB_ROOT/index.html" ]] && ! is_placeholder; then
    die "目录 $WEB_ROOT 里已有自定义 index.html，自动安装 WordPress 会覆盖它。
    请先移走该文件，或改用「自助网站」类型。"
  fi
  if [[ -n "$(ls -A "$WEB_ROOT" 2>/dev/null | grep -v '^index\.html$' || true)" ]]; then
    die "目录 $WEB_ROOT 非空（除占位页外还有其它文件），自动安装 WordPress 可能覆盖内容。
    请先备份并清空目录，或改用「自助网站」类型。"
  fi
  NEED_INSTALL_WP=1
fi

# 数据库：有 wp-config 就复用，否则准备一个可用的库名
DB_EXISTS=0
if [[ -f "$WEB_ROOT/wp-config.php" ]]; then
  CONF_DB=$(awk -F"'" '/DB_NAME/ {print $4; exit}' "$WEB_ROOT/wp-config.php" 2>/dev/null || true)
  if [[ -n "$CONF_DB" ]]; then
    DB_NAME="$CONF_DB"
    DB_EXISTS=1
    ok "沿用 wp-config.php 中的数据库：${DB_NAME}"
  fi
  CONF_USER=$(awk -F"'" '/DB_USER/ {print $4; exit}' "$WEB_ROOT/wp-config.php" 2>/dev/null || true)
  CONF_PASS=$(awk -F"'" '/DB_PASSWORD/ {print $4; exit}' "$WEB_ROOT/wp-config.php" 2>/dev/null || true)
  [[ -n "$CONF_USER" ]] && DB_USER="$CONF_USER"
  [[ -n "$CONF_PASS" ]] && DB_PASS="$CONF_PASS"
fi

if [[ "$NEEDS_DB" == "1" && "$DB_EXISTS" == "0" ]]; then
  if [[ -z "$DB_NAME" ]]; then
    if [[ "$TARGET_TYPE" == "custom" ]]; then
      DB_NAME="app_${SAFE_NAME}"
    else
      DB_NAME="wp_${SAFE_NAME}"
    fi
  fi
  case "$DB_NAME" in *[!A-Za-z0-9_]*) die "DB_NAME 只能包含字母、数字、下划线" ;; esac
  DB_USER="${DB_USER:-u_${SAFE_NAME}}"
  case "$DB_USER" in *[!A-Za-z0-9_]*) die "DB_USER 只能包含字母、数字、下划线" ;; esac
  [[ -n "$DB_PASS" ]] || DB_PASS="$(rand_pass)"

  # 同名库已被别的站点占用时换一个后缀，避免误伤
  base="$DB_NAME"
  i=1
  while :; do
    cnt=$("$DB_CLI" -N -B -e "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='${DB_NAME}';" 2>/dev/null || echo "")
    if [[ -z "$cnt" || "$cnt" == "0" ]]; then
      break
    fi
    i=$((i + 1))
    if [[ "$i" -gt 5 ]]; then
      die "数据库 ${base} 已被占用（含表），请手动指定 DB_NAME=xxx 后重试"
    fi
    DB_NAME="${base}_${i}"
  done
fi

if [[ "$DRY_RUN" == "1" ]]; then
  section "演练（DRY_RUN=1，未做任何改动）"
  echo "  站点：${DOMAIN}"
  echo "  目录：${WEB_ROOT}"
  echo "  当前类型：${CURRENT_TYPE}"
  echo "  目标类型：${TARGET_TYPE}"
  echo "  需要 PHP-FPM：$([[ "$NEEDS_PHP" == "1" ]] && echo 是 || echo 否)"
  echo "  需要数据库：$([[ "$NEEDS_DB" == "1" ]] && echo "是（${DB_NAME}）" || echo 否)"
  echo "  安装 WordPress：$([[ "$NEED_INSTALL_WP" == "1" ]] && echo 是 || echo 否)"
  echo "  证书（保留）：$([[ "$HAS_SSL" == "1" ]] && echo 有 || echo 无)"
  if [[ "$WS" == "apache" ]]; then
    echo "  会重建：${NGINX_CONF}（VirtualHost 整份重写，证书与 443 按原路径保留）"
  else
    echo "  会重写：${SNIPPET_FILE}"
    if ! grep -qF "include ${SNIPPET_FILE};" "$NGINX_CONF" 2>/dev/null; then
      echo "  还会重建：${NGINX_CONF}（原配置是内联写法，已自动备份 SSL 指令）"
    fi
  fi
  exit 0
fi

# ---------------------------------------------------------------- 备份

section "备份现有配置"

TS=$(date +%Y%m%d-%H%M%S)
BACKUP_TAR="/root/panel-site-${DOMAIN}-${TS}.tar.gz"
TAR_LIST=("${NGINX_CONF#/}")
[[ -f "$SNIPPET_FILE" ]] && TAR_LIST+=("${SNIPPET_FILE#/}")
[[ -f "$TYPE_FILE" ]] && TAR_LIST+=("${TYPE_FILE#/}")
tar czf "$BACKUP_TAR" -C / "${TAR_LIST[@]}" 2>/dev/null || die "备份 ${WS_NAME} 配置失败"
ok "已备份到 $BACKUP_TAR"

restore_backup() {
  warn "正在恢复转换前的 ${WS_NAME} 配置…" >&2
  tar xzf "$BACKUP_TAR" -C / >/dev/null 2>&1 || warn "恢复备份失败，请手动解压 $BACKUP_TAR" >&2
  # 备份里没有的、这次才创建的文件要单独清掉
  [[ "$HAD_SNIPPET" == "0" ]] && rm -f "$SNIPPET_FILE"
  [[ "$HAD_TYPE" == "0" ]] && rm -f "$TYPE_FILE"
  if ws_test >/dev/null 2>&1; then
    ws_reload >/dev/null 2>&1 || true
  fi
  if [[ "$CREATED_POOL" == "1" && -n "$POOL_FILE" ]]; then
    rm -f "$POOL_FILE" "/etc/logrotate.d/php-fpm-${SAFE_NAME}"
    systemctl reload "php${PHP_VER}-fpm" >/dev/null 2>&1 || true
  fi
  if [[ "$CREATED_DB" == "1" && -n "$CREATED_DB_NAME" ]]; then
    # 只删本次新建、且还没有表（说明没装成功）的库，避免误删数据
    cnt=$("$DB_CLI" -N -B -e "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='${CREATED_DB_NAME}';" 2>/dev/null || echo "")
    if [[ -z "$cnt" || "$cnt" == "0" ]]; then
      "$DB_CLI" -e "DROP DATABASE IF EXISTS \`${CREATED_DB_NAME}\`; DROP USER IF EXISTS '${DB_USER}'@'localhost';" >/dev/null 2>&1 || true
    else
      warn "数据库 ${CREATED_DB_NAME} 已有数据，保留不删" >&2
    fi
  fi
}

on_exit() {
  local code=$?
  [[ $code -eq 0 ]] && return 0
  if [[ "$CHANGED" == "1" ]]; then
    restore_backup
  fi
  return 0
}
trap on_exit EXIT

# ---------------------------------------------------------------- PHP socket

find_reusable_socket() {
  local f s
  while IFS= read -r f; do
    [[ "$f" == "$ENABLED_LINK" ]] && continue
    s=$(ws_socket_of "$f")
    if [[ -n "$s" && -S "$s" ]]; then printf '%s' "$s"; return 0; fi
  done < <(ws_list_confs)
  # Nginx 的能力片段里也可能挂着 socket（骨架只有 include 时）
  for f in /etc/nginx/snippets/site-*.conf; do
    [[ -f "$f" ]] || continue
    s=$(ws_socket_of "$f")
    if [[ -n "$s" && -S "$s" ]]; then printf '%s' "$s"; return 0; fi
  done
  return 1
}

if [[ "$NEEDS_PHP" == "1" ]]; then
  section "PHP-FPM"

  if [[ "$SEPARATE_POOL" == "0" ]]; then
    if PHP_SOCK=$(find_reusable_socket); then
      ok "复用现有 pool：${PHP_SOCK}（不新增常驻内存）"
    else
      PHP_SOCK=""
    fi
  fi

  if [[ -z "$PHP_SOCK" ]]; then
    POOL_NAME="site_${SAFE_NAME}"
    PHP_SOCK="/run/php/php${PHP_VER}-fpm-${SAFE_NAME}.sock"
    POOL_FILE="/etc/php/${PHP_VER}/fpm/pool.d/${POOL_NAME}.conf"
    if [[ -S "$PHP_SOCK" ]]; then
      ok "已存在可用的独立 socket：$PHP_SOCK"
    else
      [[ -e "$POOL_FILE" ]] && die "PHP pool 配置 $POOL_FILE 已存在但 socket 不存在，请先检查该文件"
      ok "创建独立 pool：${POOL_NAME}"

      cat > "$POOL_FILE" <<'POOL'
[__POOL__]
user = www-data
group = www-data

listen = __SOCK__
listen.owner = www-data
listen.group = www-data
listen.mode = 0660

pm = ondemand
pm.max_children = __MAX__
pm.process_idle_timeout = 10s
pm.max_requests = 500

chdir = __ROOT__

php_admin_value[memory_limit] = __MEM__
php_admin_value[upload_max_filesize] = 64M
php_admin_value[post_max_size] = 64M
php_admin_value[max_execution_time] = 120
php_admin_value[max_input_vars] = 3000
php_admin_flag[log_errors] = on
php_admin_value[error_log] = /var/log/php-fpm-__SAFE__.log
POOL

      sed -i \
        -e "s|__POOL__|${POOL_NAME}|g" \
        -e "s|__SOCK__|${PHP_SOCK}|g" \
        -e "s|__MAX__|${PHP_MAX_CHILDREN}|g" \
        -e "s|__ROOT__|${WEB_ROOT}|g" \
        -e "s|__MEM__|${PHP_MEMORY_LIMIT}|g" \
        -e "s|__SAFE__|${SAFE_NAME}|g" \
        "$POOL_FILE"

      touch "/var/log/php-fpm-${SAFE_NAME}.log"
      chown www-data:www-data "/var/log/php-fpm-${SAFE_NAME}.log"
      cat > "/etc/logrotate.d/php-fpm-${SAFE_NAME}" <<LOGROTATE
/var/log/php-fpm-${SAFE_NAME}.log {
    weekly
    rotate 4
    missingok
    notifempty
    compress
    delaycompress
    copytruncate
}
LOGROTATE

      CREATED_POOL=1
      if ! systemctl reload "php${PHP_VER}-fpm"; then
        warn "reload 失败，改用 restart"
        systemctl restart "php${PHP_VER}-fpm" || die "PHP-FPM 无法加载新 pool，检查 $POOL_FILE"
      fi
      for _ in 1 2 3 4 5; do
        [[ -S "$PHP_SOCK" ]] && break
        sleep 1
      done
      [[ -S "$PHP_SOCK" ]] || die "socket 未生成，检查 $POOL_FILE 与 journalctl -u php${PHP_VER}-fpm"
      ok "pool 已生效：$PHP_SOCK"
    fi
  fi
fi

# ---------------------------------------------------------------- 数据库

if [[ "$NEEDS_DB" == "1" && "$DB_EXISTS" == "0" ]]; then
  section "准备数据库"
  MYSQL_SQL="CREATE DATABASE IF NOT EXISTS \`${DB_NAME}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS '${DB_USER}'@'localhost' IDENTIFIED BY '${DB_PASS}';
ALTER USER '${DB_USER}'@'localhost' IDENTIFIED BY '${DB_PASS}';
GRANT ALL PRIVILEGES ON \`${DB_NAME}\`.* TO '${DB_USER}'@'localhost';
FLUSH PRIVILEGES;"
  "$DB_CLI" -e "$MYSQL_SQL" || die "创建数据库失败"
  CREATED_DB=1
  CREATED_DB_NAME="$DB_NAME"
  ok "数据库 ${DB_NAME} / 用户 ${DB_USER} 已就绪"
fi

# ---------------------------------------------------------------- 安装 WordPress

if [[ "$NEED_INSTALL_WP" == "1" ]]; then
  section "安装 WordPress"

  if is_placeholder; then
    rm -f "$WEB_ROOT/index.html"
    ok "已移除占位页"
  fi

  # 地址顺序由网络环境决定（国内先 cn.wordpress.org，海外先 wordpress.org）
  WP_SOURCES=()
  while IFS= read -r _wp_src; do WP_SOURCES+=("$_wp_src"); done < <(mirror_wp_sources "$WP_LOCALE")

  WP_OK=0
  for src in "${WP_SOURCES[@]}"; do
    info "下载 $src"
    if curl -fsSL --retry 2 --connect-timeout 20 -m 300 "$src" -o /tmp/wp-site.tar.gz 2>/dev/null \
       && tar -tzf /tmp/wp-site.tar.gz >/dev/null 2>&1; then
      WP_OK=1
      break
    fi
    rm -f /tmp/wp-site.tar.gz
    warn "该源不可用，尝试下一个"
  done
  [[ "$WP_OK" == "1" ]] || die "下载 WordPress 失败（所有源均不可用），请检查服务器外网"
  tar -xzf /tmp/wp-site.tar.gz -C "$WEB_ROOT" --strip-components=1
  rm -f /tmp/wp-site.tar.gz
  [[ -f "$WEB_ROOT/wp-settings.php" ]] || die "解压后缺少 wp-settings.php，压缩包不完整"

  # 解压出来的文件属主是 root，wp-cli 以 www-data 运行会写不了 uploads，必须改属主
  chown -R www-data:www-data "$WEB_ROOT"
  find "$WEB_ROOT" -type d -exec chmod 755 {} \;
  find "$WEB_ROOT" -type f -exec chmod 644 {} \;
  ok "已解压到 $WEB_ROOT（属主 www-data）"

  cat > "$WEB_ROOT/wp-config.php" <<'WPCONFIG'
<?php
define( 'DB_NAME', '__DB_NAME__' );
define( 'DB_USER', '__DB_USER__' );
define( 'DB_PASSWORD', '__DB_PASS__' );
define( 'DB_HOST', 'localhost' );
define( 'DB_CHARSET', 'utf8mb4' );
define( 'DB_COLLATE', '' );

define( 'AUTH_KEY',         '__S1__' );
define( 'SECURE_AUTH_KEY',  '__S2__' );
define( 'LOGGED_IN_KEY',    '__S3__' );
define( 'NONCE_KEY',        '__S4__' );
define( 'AUTH_SALT',        '__S5__' );
define( 'SECURE_AUTH_SALT', '__S6__' );
define( 'LOGGED_IN_SALT',   '__S7__' );
define( 'NONCE_SALT',       '__S8__' );

$table_prefix = 'wp_';

define( 'WP_DEBUG', false );
define( 'DISALLOW_FILE_EDIT', true );
define( 'FS_METHOD', 'direct' );
define( 'WP_MEMORY_LIMIT', '__MEM__' );
define( 'WP_MAX_MEMORY_LIMIT', '__MEM__' );
define( 'WP_AUTO_UPDATE_CORE', 'minor' );
define( 'WP_POST_REVISIONS', 5 );

if ( ! defined( 'ABSPATH' ) ) {
	define( 'ABSPATH', __DIR__ . '/' );
}

require_once ABSPATH . 'wp-settings.php';
WPCONFIG

  sed -i \
    -e "s|__DB_NAME__|${DB_NAME}|g" \
    -e "s|__DB_USER__|${DB_USER}|g" \
    -e "s|__DB_PASS__|${DB_PASS}|g" \
    -e "s|__MEM__|${PHP_MEMORY_LIMIT}|g" \
    -e "s|__S1__|$(openssl rand -hex 32)|" \
    -e "s|__S2__|$(openssl rand -hex 32)|" \
    -e "s|__S3__|$(openssl rand -hex 32)|" \
    -e "s|__S4__|$(openssl rand -hex 32)|" \
    -e "s|__S5__|$(openssl rand -hex 32)|" \
    -e "s|__S6__|$(openssl rand -hex 32)|" \
    -e "s|__S7__|$(openssl rand -hex 32)|" \
    -e "s|__S8__|$(openssl rand -hex 32)|" \
    "$WEB_ROOT/wp-config.php"

  chown www-data:www-data "$WEB_ROOT/wp-config.php"
  chmod 640 "$WEB_ROOT/wp-config.php"
  ok "wp-config.php 已生成"

  mkdir -p "$WP_CACHE" && chown www-data:www-data "$WP_CACHE"
  wp_cli_works() {
    [[ -s "$1" ]] || return 1
    php "$1" --info --allow-root >/dev/null 2>&1
  }
  install_wp_cli() {
    local mirrors=() _wp_url
    # 国内先走加速镜像（直连常 TLS 断流），海外直连最快、镜像只当兜底
    while IFS= read -r _wp_url; do mirrors+=("$_wp_url"); done < <(mirror_wpcli_urls)
    local tmp="/tmp/wp-cli.phar.$$" url size
    rm -f "$tmp"
    for url in "${mirrors[@]}"; do
      info "下载 wp-cli：${url:0:56}..."
      if curl -fsSL --retry 2 --connect-timeout 15 -m 180 -o "$tmp" "$url" 2>/dev/null; then
        size=$(stat -c %s "$tmp" 2>/dev/null || echo 0)
        if [[ "$size" -gt 1000000 ]] && php "$tmp" --info --allow-root >/dev/null 2>&1; then
          install -m 755 "$tmp" "$WP_BIN"
          rm -f "$tmp"
          return 0
        fi
        warn "该源下载的文件不可用（${size} 字节），换下一个"
      fi
    done
    rm -f "$tmp"
    return 1
  }

  WP_CLI_READY=0
  if wp_cli_works "$WP_BIN"; then
    WP_CLI_READY=1
  elif install_wp_cli && wp_cli_works "$WP_BIN"; then
    WP_CLI_READY=1
  fi
  [[ "$WP_CLI_READY" == "1" ]] || die "WP-CLI 不可用，无法自动安装 WordPress。请先手动安装 wp-cli 后重试"

  [[ -n "$ADMIN_PASS" ]] || ADMIN_PASS="$(rand_pass)"

  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" core install \
    --path="$WEB_ROOT" \
    --url="${SCHEME}://${DOMAIN}" \
    --title="$SITE_TITLE" \
    --admin_user="$ADMIN_USER" \
    --admin_password="$ADMIN_PASS" \
    --admin_email="$ADMIN_EMAIL" \
    --skip-email

  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    option update permalink_structure '/%postname%/' --path="$WEB_ROOT" >/dev/null
  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    rewrite flush --path="$WEB_ROOT" >/dev/null 2>&1 || true
  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    option update timezone_string 'Asia/Shanghai' --path="$WEB_ROOT" >/dev/null 2>&1 || true

  if [[ "$WP_LOCALE" == "zh_CN" ]]; then
    runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
      language core install zh_CN --activate --path="$WEB_ROOT" >/dev/null 2>&1 \
      || warn "中文语言包安装失败，可在后台切换"
  fi
  ok "WordPress 已安装"
  HAS_WP=1
fi

# ---------------------------------------------------------------- 写配置

section "切换站点能力"

CHANGED=1

if [[ "$WS" == "apache" ]]; then
  # Apache 整份重写 VirtualHost：能力指令写在里面，
  # 有证书时按原路径重建 443 块，HTTPS 不会因为换类型断掉。
  ws_apache_write_vhost "$DOMAIN" "$WEB_ROOT" "$TARGET_TYPE" "$PHP_SOCK" "$NGINX_CONF"
  ws_type_write "$DOMAIN" "$TARGET_TYPE"
  if [[ "$TARGET_TYPE" == "wordpress" ]]; then
    ws_write_wp_htaccess "$WEB_ROOT"
  else
    # 切走 WordPress 后，遗留的 .htaccess 会把请求重写到 index.php（此时 PHP 已关）
    ws_park_htaccess "$WEB_ROOT"
  fi
  ok "VirtualHost 已重写（类型：${TARGET_TYPE}）"

  [[ -e "$ENABLED_LINK" ]] || ws_enable "$DOMAIN"

  if ! WS_TEST=$(ws_test); then
    die "Apache 配置校验失败：
${WS_TEST}"
  fi
  ok "apache2ctl configtest 通过"

  ws_reload || die "${WS_NAME} 重载失败"
else
mkdir -p "$SNIPPET_DIR" "$TYPE_DIR"
write_snippet "$SNIPPET_FILE" "$TARGET_TYPE" "$PHP_SOCK"
ws_type_write "$DOMAIN" "$TARGET_TYPE"
CHANGED=1
ok "能力片段已写入：$SNIPPET_FILE"

if ! grep -qF "include ${SNIPPET_FILE};" "$NGINX_CONF" 2>/dev/null; then
  # 老配置是内联写法：抽取 SSL 与常用指令后重建骨架，保证证书不丢
  info "原配置为内联写法，正在重建为骨架 + 片段结构"

  SERVER_NAMES=$(awk '/^[[:space:]]*server_name[[:space:]]/ { sub(/^[[:space:]]*server_name[[:space:]]+/, ""); gsub(/;/, ""); print }' "$NGINX_CONF" \
    | tr ' ' '\n' | grep -v '^$' | awk '!seen[$0]++' | tr '\n' ' ' | sed 's/[[:space:]]*$//')
  [[ -n "$SERVER_NAMES" ]] || SERVER_NAMES="${DOMAIN} www.${DOMAIN}"

  CLIENT_MAX=$(awk '/client_max_body_size/ {gsub(/;/, "", $2); print $2; exit}' "$NGINX_CONF" 2>/dev/null || true)
  [[ -n "$CLIENT_MAX" ]] || CLIENT_MAX="64m"

  SSL_LINES=""
  if [[ "$HAS_SSL" == "1" ]]; then
    SSL_LINES=$(grep -hE '^[[:space:]]*(ssl_[a-z_]+|include[[:space:]]+/etc/letsencrypt/[^;]+;|add_header[[:space:]]+Strict-Transport-Security[^;]*;)' "$NGINX_CONF" \
      | sed 's/^[[:space:]]*//' | awk '!seen[$0]++' | sed 's/^/    /')
    echo "$SSL_LINES" | grep -q 'ssl_certificate_key' || die "无法从原配置提取证书私钥配置，已中止（未改动）。请先检查 $NGINX_CONF"
  fi

  {
    echo "# 由 WordPress 管理面板生成（$(date '+%Y-%m-%d %H:%M:%S')）"
    echo "# 站点能力（PHP / 静态 / WordPress）在 include 的片段里，切换类型时只重写该片段。"
    echo ""
    if [[ "$HAS_SSL" == "1" ]]; then
      cat <<EOF
server {
    listen 80;
    listen [::]:80;
    server_name ${SERVER_NAMES};

    return 301 https://\$host\$request_uri;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name ${SERVER_NAMES};

    root ${WEB_ROOT};

    client_max_body_size ${CLIENT_MAX};
    client_body_timeout 60s;

    access_log /var/log/nginx/${DOMAIN}.access.log;
    error_log  /var/log/nginx/${DOMAIN}.error.log warn;

${SSL_LINES}

    include ${SNIPPET_FILE};
}
EOF
    else
      cat <<EOF
server {
    listen 80;
    listen [::]:80;
    server_name ${SERVER_NAMES};

    root ${WEB_ROOT};

    client_max_body_size ${CLIENT_MAX};
    client_body_timeout 60s;

    access_log /var/log/nginx/${DOMAIN}.access.log;
    error_log  /var/log/nginx/${DOMAIN}.error.log warn;

    include ${SNIPPET_FILE};
}
EOF
    fi
  } > "${NGINX_CONF}.new"

  mv "${NGINX_CONF}.new" "$NGINX_CONF"
  ok "站点配置已重建（SSL：$([[ "$HAS_SSL" == "1" ]] && echo 保留 || echo 无)）"
else
  ok "站点配置已使用片段结构，无需改动"
fi

[[ -e "$ENABLED_LINK" ]] || ln -sfn "$NGINX_CONF" "$ENABLED_LINK"

if ! NGINX_TEST=$(nginx -t 2>&1); then
  die "Nginx 配置校验失败：
${NGINX_TEST}"
fi
ok "nginx -t 通过"

systemctl reload nginx || die "Nginx 重载失败"
fi

# ---------------------------------------------------------------- 收尾

if [[ "$TARGET_TYPE" != "wordpress" ]]; then
  maybe_placeholder "$TARGET_TYPE"
fi

if [[ "$TARGET_TYPE" == "static" ]]; then
  if [[ ! -e "$WEB_ROOT/index.html" && ! -e "$WEB_ROOT/index.htm" && -e "$WEB_ROOT/index.php" ]]; then
    warn "目录里目前只有 index.php（PHP 已关闭，不会被解析）。"
    warn "请上传静态首页（如 index.html），或删除旧的程序文件，否则首页会返回 403/404。"
  fi
fi

if [[ "$HAS_WP" == "1" && -x "$WP_BIN" ]]; then
  section "同步站点地址"
  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    option update siteurl "${SCHEME}://${DOMAIN}" --path="$WEB_ROOT" >/dev/null 2>&1 || true
  runuser -u www-data -- env WP_CLI_CACHE_DIR="$WP_CACHE" "$WP_BIN" \
    option update home "${SCHEME}://${DOMAIN}" --path="$WEB_ROOT" >/dev/null 2>&1 || true
  ok "siteurl / home = ${SCHEME}://${DOMAIN}"
fi

section "自检"

HTTP_CODE=$(curl -s -o /dev/null -m 15 -w '%{http_code}' -H "Host: ${DOMAIN}" "http://127.0.0.1/") || HTTP_CODE="000"
case "$HTTP_CODE" in
  200|301|302) ok "本地访问返回 ${HTTP_CODE}" ;;
  000)         warn "本地访问无响应，检查 ${WS_SERVICE}${PHP_VER:+ 与 php${PHP_VER}-fpm}" ;;
  *)           warn "返回 ${HTTP_CODE}，查看 ${WS_LOG_DIR}/${DOMAIN}.error.log" ;;
esac

{
  echo ""
  echo "── 站点能力变更 $(date '+%Y-%m-%d %H:%M:%S') ──"
  echo "类型：${CURRENT_TYPE} → ${TARGET_TYPE}（${TARGET_LABEL}）"
  if [[ "$NEEDS_DB" == "1" ]]; then
    echo "数据库：${DB_NAME}（保留）"
  else
    echo "数据库：不参与运行（数据未删除，切回 PHP 类型即可继续使用）"
  fi
  if [[ "$WS" == "apache" ]]; then
    echo "站点配置：${NGINX_CONF}"
  else
    echo "Nginx 片段：${SNIPPET_FILE}"
  fi
} >> "$INFO_FILE" 2>/dev/null || true
chmod 600 "$INFO_FILE" 2>/dev/null || true

section "完成"
cat <<SUMMARY

  站点：${DOMAIN}
  类型：${CURRENT_LABEL} → ${TARGET_LABEL}
  地址：${SCHEME}://${DOMAIN}
  目录：${WEB_ROOT}（内容未改动）
  备份：${BACKUP_TAR}

$(if [[ "$TARGET_TYPE" == "static" ]]; then
    echo "提示：PHP 已关闭，.php 请求直接返回 404。数据库仍在，切回其它类型即可继续使用。"
  elif [[ "$TARGET_TYPE" == "custom" ]]; then
    if [[ "$WS" == "apache" ]]; then
      echo "提示：PHP 已启用，上传程序后即可运行；伪静态可在 ${WEB_ROOT}/.htaccess 里追加。"
    else
      echo "提示：PHP 已启用，上传程序后即可运行；伪静态可在 ${SNIPPET_FILE} 里追加。"
    fi
  elif [[ "$NEED_INSTALL_WP" == "1" ]]; then
    echo "后台：${SCHEME}://${DOMAIN}/wp-admin/"
    echo "账号：${ADMIN_USER} / ${ADMIN_PASS}"
  else
    echo "提示：WordPress 站点规则已启用，原有数据未改动。"
  fi)

SUMMARY
