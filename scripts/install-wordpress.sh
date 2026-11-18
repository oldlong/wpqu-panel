#!/usr/bin/env bash
#
# WordPress 一键部署脚本（面向 2核2G 级云服务器）
# 组件：Debian 12/13 或 Ubuntu 22.04/24.04 + Web 服务器 + PHP-FPM + 数据库 + Redis(可选)
#
# 用法：
#   bash install-wordpress.sh                          # 交互式（默认组合）
#   DOMAIN=example.com ADMIN_EMAIL=me@example.com bash install-wordpress.sh
#
#   自定义选装示例（Apache + PHP 8.3 + MySQL 5.7）：
#   DOMAIN=example.com WEB_SERVER=apache PHP_VERSION=8.3 DB_ENGINE=mysql57 \
#     bash install-wordpress.sh
#
# 可选环境变量：
#   DOMAIN            站点域名（必填）
#   SITE_TITLE        站点标题，默认 "My WordPress Site"
#   ADMIN_USER        后台用户名，默认 admin
#   ADMIN_PASS        后台密码，留空则自动生成
#   ADMIN_EMAIL       管理员邮箱，默认 admin@$DOMAIN
#   WP_LOCALE         zh_CN 或 en_US，默认 zh_CN
#   DB_NAME/DB_USER   数据库名/用户，默认按域名派生：wp_<片段> / u_<片段>
#   DB_PASS           数据库密码，留空则自动生成
#   WEB_SERVER        Web 服务器：nginx（默认）或 apache
#                     apache = Apache 2.4 + mpm_event + php-fpm（不是 mod_php）
#                     两者都占 80 端口，脚本会停用另一方的服务
#   PHP_VERSION       PHP 版本，如 8.1 / 8.2 / 8.3 / 8.4；留空用发行版默认
#                     系统源里没有的版本会自动添加 Sury（Debian）或 ondrej（Ubuntu）源
#   DB_ENGINE         数据库引擎，三选一：
#                     mariadb（默认，省内存，推荐）
#                     mysql84（MySQL 8.4 LTS，Oracle 官方源）
#                     mysql57（MySQL 5.7，已 EOL；仅在必须对齐旧环境时选，
#                              低配主机用它并不比 MariaDB 省内存）
#                     三者不能共存；切换前必须清空已有库，脚本不会替你删数据
#   PHP_MEMORY_LIMIT  默认 128M（内容站可 256M，2G 机器不建议更高）
#   PHP_MAX_CHILDREN  默认 8
#   INSTALL_REDIS     1/0，默认 0（对象缓存不是必需品；Debian 13 自带的 8.0.2
#                     已被发行版回溯修补 CVE-2025-49844 / CVE-2025-32023，但版本号
#                     仍会被云安全扫描判为漏洞。需要时务必用 >= 8.0.4）
#   ENABLE_SSL        1/0，默认 1（域名未解析时会自动跳过）
#   ENABLE_UFW        1/0，默认 1
#   SSH_PORT          SSH 端口，默认 22（开防火墙前务必确认）
#   APT_MIRROR        设置后把 apt 源替换为国内镜像，如 https://mirrors.aliyun.com
#
# 要求：root 权限，全新服务器，域名已解析到本机
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
  echo "请用 root 运行：sudo -i 后重新执行，或 sudo bash $0" >&2
  exit 1
fi

trap 'echo -e "\n[ERROR] 第 $LINENO 行命令失败（退出码 $?），部署中止。日志：${LOG:-未创建}" >&2' ERR

# ---------------------------------------------------------------- 基础工具函数

LOG="/var/log/wp-deploy-$(date +%Y%m%d-%H%M%S).log"
mkdir -p /var/log
exec > >(tee -a "$LOG") 2>&1

C_RESET='\033[0m'; C_INFO='\033[0;36m'; C_OK='\033[0;32m'; C_WARN='\033[0;33m'; C_ERR='\033[0;31m'

info() { echo -e "${C_INFO}[*]${C_RESET} $*"; }
ok()   { echo -e "${C_OK}[+]${C_RESET} $*"; }
warn() { echo -e "${C_WARN}[!]${C_RESET} $*"; }
die()  { echo -e "${C_ERR}[x]${C_RESET} $*" >&2; exit 1; }

section() {
  echo ""
  echo -e "${C_INFO}────────────────────────────────────────────────────────${C_RESET}"
  echo -e "${C_INFO} $* ${C_RESET}"
  echo -e "${C_INFO}────────────────────────────────────────────────────────${C_RESET}"
}

# 生成随机密码：不要用 `tr </dev/urandom | head -c N`，head 提前退出会让 tr 收到
# SIGPIPE（退出码 141），在 pipefail 下直接终止脚本。这里改用 openssl 一次取够，
# 再用 bash 子串截断，全程不带会提前关闭的管道。
rand_pass() {
  local raw
  if command -v openssl >/dev/null 2>&1; then
    raw=$(openssl rand -hex 32)
  else
    raw=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
  fi
  printf '%s' "${raw:0:20}"
}

# 遇到配置文件冲突时保留现有版本，避免 dpkg 弹交互把无人值守的部署卡死
DPKG_OPTS=(-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold)

apt_wait() {
  local i=0
  command -v fuser >/dev/null 2>&1 || return 0
  while fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/lib/apt/lists/lock >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -gt 90 ]; then
      die "等待 apt/dpkg 锁超时，请先结束 cloud-init 或手动 apt 进程"
    fi
    sleep 2
  done
}

apt_install() {
  apt_wait
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${DPKG_OPTS[@]}" "$@"
}

apt_install_optional() {
  apt_wait
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${DPKG_OPTS[@]}" "$@" \
    || warn "可选组件安装失败（不影响主流程）：$*"
}

# ---------------------------------------------------------------- 参数收集

DOMAIN="${DOMAIN:-}"
SITE_TITLE="${SITE_TITLE:-My WordPress Site}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASS="${ADMIN_PASS:-}"
ADMIN_EMAIL="${ADMIN_EMAIL:-}"
WP_LOCALE="${WP_LOCALE:-zh_CN}"
# 库名与账号按域名派生（见下方 DOMAIN 规范化之后），这里不能用固定名字：
# 同一台机器上装第二个站时，固定名会让两个站共用一个库，还会把库账号密码改掉，
# 已有站点会立刻数据库连接失败。留空表示「跟着域名走」。
DB_NAME="${DB_NAME:-}"
DB_USER="${DB_USER:-}"
DB_PASS="${DB_PASS:-}"
WEB_SERVER="${WEB_SERVER:-nginx}"
PHP_VERSION="${PHP_VERSION:-}"
DB_ENGINE="${DB_ENGINE:-mariadb}"
PHP_MEMORY_LIMIT="${PHP_MEMORY_LIMIT:-128M}"
PHP_MAX_CHILDREN="${PHP_MAX_CHILDREN:-8}"
INSTALL_REDIS="${INSTALL_REDIS:-0}"
ENABLE_SSL="${ENABLE_SSL:-1}"
ENABLE_UFW="${ENABLE_UFW:-1}"
APT_MIRROR="${APT_MIRROR:-}"

# 未指定 SSH 端口时，从 sshd 实际监听推断，避免开防火墙后把自己关在门外
if [[ -z "${SSH_PORT:-}" ]]; then
  SSH_PORT=""
  if command -v ss >/dev/null 2>&1; then
    SSH_PORT=$(ss -lntp 2>/dev/null | awk '/sshd/ {n=split($4,a,":"); p=a[n]} END {if (p != "") print p}')
  fi
  SSH_PORT="${SSH_PORT:-22}"
fi

_ans=""
if [[ -t 0 ]]; then
  if [[ -z "$DOMAIN" ]]; then
    read -r -p "站点域名（如 example.com）: " DOMAIN || true
  fi
  read -r -p "站点标题 [$SITE_TITLE]: " _ans || true
  SITE_TITLE="${_ans:-$SITE_TITLE}"
  _ans=""
  if [[ -z "$ADMIN_EMAIL" ]]; then
    read -r -p "管理员邮箱 [admin@$DOMAIN]: " _ans || true
    ADMIN_EMAIL="${_ans:-admin@$DOMAIN}"
  fi
  _ans=""
  read -r -p "是否启用 HTTPS 自动申请 [Y/n]: " _ans || true
  case "${_ans:-Y}" in y|Y|yes|YES) ENABLE_SSL=1 ;; *) ENABLE_SSL=0 ;; esac
fi

[[ -n "$DOMAIN" ]] || die "必须提供域名。示例：DOMAIN=example.com bash $0"
DOMAIN="${DOMAIN#http://}"; DOMAIN="${DOMAIN#https://}"; DOMAIN="${DOMAIN%%/*}"
[[ -n "$ADMIN_EMAIL" ]] || ADMIN_EMAIL="admin@$DOMAIN"

# 库名与账号默认跟域名走：wp_<域名片段> / u_<域名片段>。
# 固定成 wordpress / wpuser 会让同机的第二个站直接写进第一个站的库，
# 还会 ALTER 掉它的账号密码，把已有站点整挂。
if [[ -z "$DB_NAME" || -z "$DB_USER" ]]; then
  SAFE_NAME="${DOMAIN//[.-]/_}"
  SAFE_NAME="${SAFE_NAME:0:12}"
  [[ -n "$DB_NAME" ]] || DB_NAME="wp_${SAFE_NAME}"
  [[ -n "$DB_USER" ]] || DB_USER="u_${SAFE_NAME}"
fi

# WordPress 的 is_email() 要求域名含点且顶级域至少 2 位，单段主机名会直接失败
if ! [[ "$ADMIN_EMAIL" =~ ^[^@[:space:]]+@[^@[:space:]]+\.[A-Za-z]{2,}$ ]]; then
  warn "管理员邮箱 ${ADMIN_EMAIL} 不是合法邮箱，改用 admin@example.com（安装后可在后台修改）"
  ADMIN_EMAIL="admin@example.com"
fi
[[ -n "$ADMIN_PASS" ]]  || ADMIN_PASS="$(rand_pass)"
[[ -n "$DB_PASS" ]]     || DB_PASS="$(rand_pass)"

case "$DB_NAME" in *[!A-Za-z0-9_]*) die "DB_NAME 只能包含字母、数字、下划线" ;; esac
case "$DB_USER" in *[!A-Za-z0-9_]*) die "DB_USER 只能包含字母、数字、下划线" ;; esac

case "$WEB_SERVER" in
  nginx)  WEB_LABEL="Nginx"; WEB_SERVICE="nginx" ;;
  apache) WEB_LABEL="Apache 2.4"; WEB_SERVICE="apache2" ;;
  *) die "WEB_SERVER 只能是 nginx 或 apache，当前为：${WEB_SERVER}" ;;
esac

# PHP 版本留空 = 用发行版默认（Debian 13 是 8.4）；指定版本则按版本号装
if [[ -n "$PHP_VERSION" ]]; then
  [[ "$PHP_VERSION" =~ ^[0-9]+\.[0-9]+$ ]] \
    || die "PHP_VERSION 形如 8.3，当前为：${PHP_VERSION}"
  case "$PHP_VERSION" in
    7.[0-9]|8.*) ;;
    *) die "PHP 版本只能是 7.x / 8.x，当前为：${PHP_VERSION}" ;;
  esac
fi

case "$DB_ENGINE" in
  mariadb)         DB_LABEL="MariaDB";       DB_FAMILY="mariadb" ;;
  mysql8|mysql84)  DB_ENGINE="mysql84"; DB_LABEL="MySQL 8.4 LTS"; DB_FAMILY="mysql" ;;
  mysql57)         DB_LABEL="MySQL 5.7";     DB_FAMILY="mysql" ;;
  *) die "DB_ENGINE 只能是 mariadb / mysql84 / mysql57，当前为：${DB_ENGINE}" ;;
esac

if [[ "$DB_ENGINE" == "mysql57" ]]; then
  warn "MySQL 5.7 已于 2023-10 停止维护，仅建议在必须对齐旧业务时选择"
  warn "论低配省内存，MariaDB 反而更轻；5.7 在 Debian 12/13 上还要额外补老依赖"
fi

# MariaDB 与 MySQL 的命令名不同（Debian 上 mariadb-* 与 mysql-* 互为软链，但
# 装 MySQL 8 时只有 mysql-*），这里统一探测一次，后面所有调用都走 $DB_CLI
DB_CLI=$(command -v mariadb || command -v mysql || true)
DB_DUMP=$(command -v mariadb-dump || command -v mysqldump || true)

WEB_ROOT="/var/www/${DOMAIN}"
INFO_FILE="/root/wp-deploy-info.txt"
WP_BIN="/usr/local/bin/wp"

# ---------------------------------------------------------------- 前置检查

section "前置检查"

[[ -f /etc/os-release ]] || die "无法识别系统版本"
. /etc/os-release
case "${ID:-}" in
  debian|ubuntu) ok "系统：${PRETTY_NAME}" ;;
  *) die "仅支持 Debian / Ubuntu，当前为 ${ID:-unknown}。CentOS 系请改用别的方式。" ;;
esac

MEM_MB=$(awk '/MemTotal/{printf "%d", $2/1024}' /proc/meminfo)
CPU=$(nproc)
ok "CPU ${CPU} 核，内存 ${MEM_MB} MB"
if [[ "$MEM_MB" -lt 900 ]]; then
  die "内存不足 1G，无法稳定运行 LEMP + WordPress"
fi
if [[ "$MEM_MB" -lt 1700 ]]; then
  warn "内存小于 1.7G，建议把 PHP_MAX_CHILDREN 降到 4"
fi

if command -v ss >/dev/null 2>&1 && ss -lntp 2>/dev/null | awk '$4 ~ /:80$/ {f=1} END {exit !f}'; then
  warn "80 端口已被占用，若不是 cloud-init 临时进程，Nginx 启动可能失败"
fi

# 域名解析校验（不阻塞）
if command -v getent >/dev/null 2>&1; then
  RESOLVED=$(getent hosts "$DOMAIN" 2>/dev/null | awk 'NR==1 {print $1}') || true
  if [[ -z "$RESOLVED" ]]; then
    warn "域名 $DOMAIN 当前无法解析，SSL 申请会跳过，稍后自行执行 certbot"
  else
    ok "域名解析到 $RESOLVED"
  fi
fi

# ---------------------------------------------------------------- 可选换源

if [[ -n "$APT_MIRROR" ]]; then
  section "替换 apt 源为 $APT_MIRROR"
  MIRROR_HOST="${APT_MIRROR#*//}"
  MIRROR_HOST="${MIRROR_HOST%%/*}"
  STAMP=$(date +%s)
  for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.sources; do
    [[ -f "$f" ]] || continue
    cp -a "$f" "${f}.bak.${STAMP}"
    sed -i \
      -e "s|security.debian.org|${MIRROR_HOST}|g" \
      -e "s|deb.debian.org|${MIRROR_HOST}|g" \
      -e "s|archive.ubuntu.com|${MIRROR_HOST}|g" \
      -e "s|security.ubuntu.com|${MIRROR_HOST}|g" \
      "$f"
    ok "已处理 $f"
  done
fi

# ---------------------------------------------------------------- 系统准备

section "系统更新与基础配置"

export DEBIAN_FRONTEND=noninteractive
apt_wait

# 先把软件源换成就近镜像：海外机器指着阿里云、国内机器指着 deb.debian.org，
# 都会让后面几十次 apt 安装慢到离谱。换完会先验证，不通就自动撤回。
mirror_init
ok "网络环境：$MIRROR_SUMMARY"
if ! mirror_apt_switch; then
  warn "$MIRROR_APT_ERROR"
elif [[ "$MIRROR_APT_CHANGED" == "1" ]]; then
  ok "软件源已切到就近镜像"
fi

# 第三方源（比如上次跑残留的 MySQL 源）签名失效能让 apt-get update 直接返回非 0，
# 这里不因此中止：真正装数据库前会重写源并再 update 一次
if ! mirror_apt_update 1 2>/tmp/apt-update-early.log; then
  warn "apt update 有仓库报错（继续执行，装数据库前会重试）："
  grep -E '^(E|W):' /tmp/apt-update-early.log 2>/dev/null | head -4 | sed 's/^/    /' >&2
fi
rm -f /tmp/apt-update-early.log
apt-get -y -qq "${DPKG_OPTS[@]}" upgrade || warn "系统升级有告警，继续"

apt_install curl ca-certificates gnupg lsb-release unzip tar wget

timedatectl set-timezone Asia/Shanghai 2>/dev/null || true
ok "时区：$(timedatectl show -p Timezone --value 2>/dev/null || echo Asia/Shanghai)"

# swap：2G 内存必备兜底
SWAP_SHOW=$(swapon --show 2>/dev/null || true)
if [[ "$SWAP_SHOW" != *"/swapfile"* ]]; then
  info "创建 2G swap"
  if [[ ! -f /swapfile ]]; then
    fallocate -l 2G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=2048 status=none
    chmod 600 /swapfile
    mkswap /swapfile >/dev/null
  fi
  swapon /swapfile
  grep -q '^/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
  ok "swap 已启用"
else
  ok "swap 已存在，跳过"
fi

cat > /etc/sysctl.d/99-wordpress.conf <<'SYSCTL'
vm.swappiness = 10
vm.vfs_cache_pressure = 50
net.core.somaxconn = 1024
net.ipv4.tcp_fin_timeout = 20
SYSCTL
sysctl -p /etc/sysctl.d/99-wordpress.conf >/dev/null
ok "内核参数已写入 /etc/sysctl.d/99-wordpress.conf"

# ---------------------------------------------------------------- 数据库安装

# MariaDB / MySQL 8.4 / MySQL 5.7 共用 3306 与 /var/lib/mysql，一次只能装一个。
# 卸载会连数据目录一起删，所以先确认没有业务库；有就停手，绝不替用户删数据。
# 判断包是否真的装上了（dpkg -l 对已知但未安装的包也返回 0，容易误判）
pkg_installed() {
  dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q 'install ok installed'
}

engine_label() {
  case "$1" in
    mariadb) printf 'MariaDB' ;;
    mysql84) printf 'MySQL 8.4' ;;
    mysql57) printf 'MySQL 5.7' ;;
    mysql)   printf 'MySQL' ;;
    *)       printf '未安装' ;;
  esac
}

# 当前装的是哪种引擎（含版本），没装输出 none
db_current_engine() {
  if pkg_installed mariadb-server; then
    printf 'mariadb'
    return 0
  fi
  if pkg_installed mysql-community-server || pkg_installed mysql-server-8.4 || pkg_installed mysql-server; then
    local v=""
    v=$(dpkg-query -W -f='${Version}' mysql-community-server 2>/dev/null || true)
    [[ -n "$v" ]] || v=$(dpkg-query -W -f='${Version}' mysql-server-8.4 2>/dev/null || true)
    case "$v" in
      5.7*) printf 'mysql57' ;;
      8.*)  printf 'mysql84' ;;
      *)    printf 'mysql' ;;
    esac
    return 0
  fi
  printf 'none'
}

purge_db_engine() {
  local cur="$1" user_dbs bak f
  local -a pkgs=()

  case "$cur" in
    mariadb) pkgs=(mariadb-server mariadb-client mariadb-common) ;;
    *)       pkgs=(mysql-server mysql-server-8.4 mysql-community-server \
                  mysql-client mysql-community-client mysql-community-client-core \
                  mysql-community-client-plugins mysql-community-server-core mysql-common) ;;
  esac

  warn "服务器上已安装 $(engine_label "$cur")，与目标 ${DB_LABEL} 冲突，需要先卸载"

  if [[ -n "${DB_CLI:-}" ]]; then
    user_dbs=$("$DB_CLI" -N -B -e 'SHOW DATABASES' 2>/dev/null \
      | grep -vE '^(information_schema|performance_schema|mysql|sys)$' | tr '\n' ' ' || true)
    if [[ -n "${user_dbs// /}" ]]; then
      die "库里还有业务数据：${user_dbs}
    切换数据库引擎会删除这些库，脚本不会替你删。请先用面板「备份」或 mysqldump 导出，
    确认可以丢弃后手工 apt purge 旧引擎，再重跑本脚本。"
    fi
  fi

  systemctl stop mysql >/dev/null 2>&1 || true
  systemctl stop mariadb >/dev/null 2>&1 || true
  systemctl disable mysql >/dev/null 2>&1 || true
  systemctl disable mariadb >/dev/null 2>&1 || true

  # 脚本/面板写进去的调优参数不属于任何包，purge 不会带走。里面可能有
  # 特定版本才认的参数（如 MySQL 8.4 的 innodb_redo_log_capacity），
  # 留给 5.7 会让它直接起不来，所以先打包收走。
  for f in /etc/mysql/*/99-wordpress.cnf; do
    [[ -f "$f" ]] || continue
    mv "$f" "${f}.bak-$(date +%Y%m%d-%H%M%S)"
  done

  DEBIAN_FRONTEND=noninteractive apt-get purge -y \
    -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold \
    "${pkgs[@]}" >/dev/null 2>&1 || true
  rm -f /etc/mysql/debian.cnf

  # 残留的数据目录会让新引擎初始化失败，挪走而不是删掉
  if [[ -d /var/lib/mysql && -n "$(ls -A /var/lib/mysql 2>/dev/null)" ]]; then
    bak="/var/lib/mysql.${cur}-backup-$(date +%Y%m%d-%H%M%S)"
    mv /var/lib/mysql "$bak"
    warn "原数据目录已改名保留：$bak（确认无用后自行删除）"
  fi
  ok "已卸载 $(engine_label "$cur")"
}

# 目标引擎与现装的一致就沿用；不一致才卸载（含 8.4 ↔ 5.7 这种同族换版本）
prepare_db_engine() {
  local want="$1" cur
  cur=$(db_current_engine)
  if [[ "$cur" == "$want" ]]; then
    ok "已安装 $(engine_label "$cur")，沿用现有实例"
    return 0
  fi
  [[ "$cur" == "none" ]] && return 0
  purge_db_engine "$cur"
}

install_mariadb() {
  prepare_db_engine mariadb
  # 切到 MariaDB 后 MySQL 官方源没用了，去掉免得每次 apt update 都多连一次
  rm -f /etc/apt/sources.list.d/mysql.list
  apt_install mariadb-server mariadb-client
  DB_SERVICE="mariadb"
  # 统一写 conf.d：三种引擎的 /etc/mysql/my.cnf 都会 include 它；
  # mariadb.conf.d / mysql.conf.d 是否被 include 取决于 alternatives 当前指向
  # 哪个 my.cnf（purge 过数据库后常退化成 my.cnf.fallback，只 include conf.d）
  DB_CONF_DIR="/etc/mysql/conf.d"
}

# Oracle 的签名密钥会过期并换文件名重新发布（同一个 key ID，只延期）。
# 2023 那版已在 2025-10-22 过期，直接用会让 apt 拒收整个仓库，所以按新旧顺序试。
mysql_keyring() {
  apt_install gnupg curl ca-certificates
  install -d -m 755 /usr/share/keyrings
  local key_name="" key_tmp
  key_tmp=$(mktemp)
  for key_name in RPM-GPG-KEY-mysql-2025 RPM-GPG-KEY-mysql-2023 RPM-GPG-KEY-mysql; do
    if curl -fsSL --max-time 20 "https://repo.mysql.com/${key_name}" -o "$key_tmp" 2>/dev/null \
       && [[ -s "$key_tmp" ]]; then
      break
    fi
    key_name=""
  done
  [[ -n "$key_name" ]] || die "下载 MySQL 源签名失败，检查服务器能否访问 repo.mysql.com"
  gpg --dearmor --yes -o /usr/share/keyrings/mysql.gpg < "$key_tmp" \
    || die "导入 MySQL 源签名失败（$key_name）"
  rm -f "$key_tmp"
  chmod 644 /usr/share/keyrings/mysql.gpg
}

install_mysql84() {
  prepare_db_engine mysql84
  mysql_keyring

  local codename
  codename=$(. /etc/os-release && echo "${VERSION_CODENAME:-bookworm}")
  # 官方源不一定覆盖刚发布的发行版（Debian/Ubuntu 新版本常见），没有就退到 bookworm
  if ! curl -fsI --max-time 20 "https://repo.mysql.com/apt/debian/dists/${codename}/Release" >/dev/null 2>&1; then
    warn "MySQL 官方源没有 ${codename} 的仓库，回退到 bookworm 组件（glibc 兼容）"
    codename="bookworm"
  fi

  cat > /etc/apt/sources.list.d/mysql.list <<REPO
# MySQL 官方 APT 源（由 install-wordpress.sh 写入）
deb [signed-by=/usr/share/keyrings/mysql.gpg] https://repo.mysql.com/apt/debian/ ${codename} mysql-8.4-lts
REPO

  apt_wait
  apt-get update -qq 2>&1 | tee /tmp/mysql-apt-update.log >/dev/null || true
  # 签名过期时的报错是 "is not signed / is not live"，继续装只会得到
  # "no installation candidate"，这里直接给出可读的原因
  if grep -qiE 'repo\.mysql\.com.*(not signed|NO_PUBKEY|is not live|Expired)' /tmp/mysql-apt-update.log; then
    die "MySQL 官方源的 GPG 签名校验失败（Oracle 又轮换了密钥，脚本里的 key 名已过期）。
    临时办法：把 DB_ENGINE 改成 mariadb 重跑，或手动更新 /usr/share/keyrings/mysql.gpg。
    原始报错：$(grep -iE 'repo\.mysql\.com' /tmp/mysql-apt-update.log | head -2 | tr '\n' ' ')"
  fi
  rm -f /tmp/mysql-apt-update.log
  apt_install mysql-server mysql-client

  # 有些机器上装过 Oracle tarball 版 MySQL（/usr/local/mysql-generic-*），
  # /usr/bin/mysql 可能被指到那上面，默认 socket 是 /tmp/mysql.sock，
  # 之后所有命令都会「连不上数据库」却看不出原因。这里核对并修回包的版本。
  if ! mysql --version 2>/dev/null | grep -qE 'Ver 8\.'; then
    warn "mysql 客户端不是 8.x：$(mysql --version 2>/dev/null | head -1)"
    warn "多半是残留的旧 MySQL 抢占了 /usr/bin/mysql，重装客户端包修正"
    rm -f /usr/bin/mysql
    apt-get install -y --reinstall mysql-community-client-core mysql-community-client >/dev/null 2>&1 || true
    mysql --version 2>/dev/null | grep -qE 'Ver 8\.' \
      || warn "客户端仍是：$(command -v mysql)（$(mysql --version 2>/dev/null | head -1)）"
  fi

  DB_SERVICE="mysql"
  DB_CONF_DIR="/etc/mysql/conf.d"
  ok "MySQL 8.4 已安装（源：repo.mysql.com/${codename}）"
}

# MySQL 5.7：Oracle 只为 Debian 10(buster) 出过包。在 Debian 12/13 上直接加
# buster 源，apt 会顺手把一批老库一起拉进来；所以只在临时清单里取 .deb 手工装，
# 系统源保持干净。实测 5.7.42 的二进制在新 glibc 上能跑，唯一缺的是
# libaio.so.1（Debian 13 已改名 libaio1t64），补一个 bullseye 的 libaio1 即可共存。
install_mysql57() {
  [[ "${ID:-}" == "debian" ]] || die "MySQL 5.7 只在 Debian 上可用（Oracle 只为 Debian 10 出过包）。
    Ubuntu 请改选 MariaDB 或 MySQL 8.4。"
  prepare_db_engine mysql57
  mysql_keyring

  local arch
  arch=$(dpkg --print-architecture)
  [[ "$arch" == "amd64" ]] || die "MySQL 5.7 的官方包只有 amd64，当前是 ${arch}；请改选 MariaDB 或 MySQL 8.4"
  local tmp=/root/mysql57-debs base="https://repo.mysql.com/apt/debian"
  rm -rf "$tmp"
  mkdir -p "$tmp"

  info "读取 MySQL 5.7 软件包清单"
  curl -fsSL --max-time 90 "${base}/dists/buster/mysql-5.7/binary-amd64/Packages.gz" \
    -o /tmp/mysql57-packages.gz \
    || die "下载 MySQL 5.7 包清单失败，检查服务器能否访问 repo.mysql.com"
  gzip -dc /tmp/mysql57-packages.gz > /tmp/mysql57-packages
  rm -f /tmp/mysql57-packages.gz

  local -a debs=()
  # 老 glibc 的 libaio，只有缺 libaio.so.1 时才补
  if [[ ! -e /usr/lib/x86_64-linux-gnu/libaio.so.1 && ! -e /lib/x86_64-linux-gnu/libaio.so.1 ]]; then
    info "补装 libaio1（MySQL 5.7 依赖 libaio.so.1）"
    curl -fsSL --max-time 60 \
      "https://deb.debian.org/debian/pool/main/liba/libaio/libaio1_0.3.112-9_${arch}.deb" \
      -o "$tmp/libaio1.deb" || die "下载 libaio1 失败"
    debs+=("$tmp/libaio1.deb")
  fi
  apt_install libncurses6

  local pkg path
  # 顺序有讲究：common → client → mysql-client 元包 → server
  for pkg in mysql-common mysql-community-client mysql-client mysql-community-server; do
    path=$(awk -v want="Package: ${pkg}" \
      'BEGIN { RS="\n\n" } $0 ~ ("^" want "\n") { for (i=1;i<=NF;i++) if ($i == "Filename:") { print $(i+1); exit } }' \
      /tmp/mysql57-packages)
    [[ -n "$path" ]] || die "包清单里找不到 ${pkg}，Oracle 可能已下线 5.7 仓库"
    info "下载 ${pkg}"
    curl -fsSL --max-time 300 "${base}/${path}" -o "$tmp/$(basename "$path")" \
      || die "下载 ${pkg} 失败"
    debs+=("$tmp/$(basename "$path")")
  done
  rm -f /tmp/mysql57-packages

  apt_wait
  DEBIAN_FRONTEND=noninteractive dpkg -i "${debs[@]}" > /tmp/mysql57-dpkg.log 2>&1 || true
  if ! dpkg-query -W -f='${Status}' mysql-community-server 2>/dev/null | grep -q 'install ok installed'; then
    grep -E 'dependency|error|未安装' /tmp/mysql57-dpkg.log | head -8 | sed 's/^/    /' >&2 || true
    # 少数机器上 libgcc1 之类的虚拟包名对不上，补齐后可继续
    DEBIAN_FRONTEND=noninteractive apt-get -f install -y >/dev/null 2>&1 || true
    dpkg --configure -a >/dev/null 2>&1 || true
  fi
  rm -f /tmp/mysql57-dpkg.log
  dpkg-query -W -f='${Status}' mysql-community-server 2>/dev/null | grep -q 'install ok installed' \
    || die "MySQL 5.7 安装未完成，检查上面的 dpkg 报错"

  systemctl enable mysql >/dev/null 2>&1 || true
  systemctl restart mysql || die "MySQL 5.7 启动失败：journalctl -u mysql -n 50"

  DB_SERVICE="mysql"
  # 5.7 的 /etc/mysql/my.cnf 经 alternatives 指向 my.cnf.fallback，只 include
  # conf.d/，mysql.conf.d/ 里的默认配置和我们的调优都不会被读到，所以写 conf.d
  DB_CONF_DIR="/etc/mysql/conf.d"
  mkdir -p "$DB_CONF_DIR"
  ok "MySQL 5.7 已安装（源：repo.mysql.com/buster，二进制兼容 ${PRETTY_NAME}）"
}

# ---------------------------------------------------------------- Web 服务器与 PHP

pkg_available() {
  local pol
  pol=$(apt-cache policy "$1" 2>/dev/null)
  case "$pol" in
    *"Candidate: (none)"*) return 1 ;;
    *"Candidate: "*) return 0 ;;
  esac
  return 1
}

php_repo_added=0
# packages.sury.org 在境外，国内 1–5Mbps 的小机器装一套 PHP 能等半小时以上。
# 优先用 USTC 镜像（签名文件齐全，signed-by 照样校验），失败再回官方源。
PHP_REPO_MIRROR="https://mirrors.ustc.edu.cn/sury/php/"
PHP_REPO_OFFICIAL="https://packages.sury.org/php/"

# add_php_repo [源地址]；不带参数时自动挑一个可用的
add_php_repo() {
  local want="${1:-}" base="" cand
  if [[ "$php_repo_added" == "1" && -z "$want" ]]; then
    return 0
  fi

  if [[ "${ID:-}" == "ubuntu" ]]; then
    section "添加 PHP 软件源"
    apt_install software-properties-common
    add-apt-repository -y ppa:ondrej/php >/dev/null 2>&1 \
      || die "添加 ondrej/php PPA 失败（检查网络或系统版本是否受支持）"
    apt_wait
    mirror_apt_update 2 || die "apt update 失败，PHP 源不可用"
    php_repo_added=1
    return 0
  fi

  local key=/usr/share/keyrings/sury-php.gpg
  if [[ ! -s "$key" ]]; then
    section "添加 PHP 软件源"
    info "下载 Sury 签名密钥"
    curl -fsSL --max-time 30 "${PHP_REPO_OFFICIAL}apt.gpg" -o "$key" \
      || die "下载 Sury 密钥失败（服务器可能访问不了 packages.sury.org）"
    chmod 644 "$key"
  fi
  [[ -n "${VERSION_CODENAME:-}" ]] || die "读不到系统代号，无法配置 Sury 源"

  if [[ -n "$want" ]]; then
    base="$want"
  else
    for cand in "$PHP_REPO_MIRROR" "$PHP_REPO_OFFICIAL"; do
      if curl -fsI --max-time 8 "${cand}dists/${VERSION_CODENAME}/Release" >/dev/null 2>&1; then
        base="$cand"
        break
      fi
    done
    [[ -n "$base" ]] || die "找不到可用的 PHP 软件源（镜像与官方源都不通）"
  fi

  section "添加 PHP 软件源"
  echo "deb [signed-by=${key}] ${base} ${VERSION_CODENAME} main" \
    > /etc/apt/sources.list.d/sury-php.list
  ok "已写入 /etc/apt/sources.list.d/sury-php.list（${base}${VERSION_CODENAME}）"

  apt_wait
  mirror_apt_update 2 || die "apt update 失败，PHP 源不可用"
  php_repo_added=1
}

install_php() {
  local want="$1" pkg ver
  if [[ -n "$want" ]]; then
    if ! pkg_available "php${want}-fpm"; then
      info "系统源里没有 PHP ${want}，改用第三方 PHP 源"
      add_php_repo
    fi
    # 镜像同步有延迟，刚发布的版本可能还没镜像过来，这时回官方源再找一次
    if ! pkg_available "php${want}-fpm" && [[ "${ID:-}" == "debian" ]]; then
      warn "当前 PHP 源里没有 ${want}，改用官方源重试"
      add_php_repo "$PHP_REPO_OFFICIAL"
    fi
    pkg_available "php${want}-fpm" || die "软件源里没有 php${want}-fpm，无法安装 PHP ${want}"
    PHP_VER="$want"
  else
    # 不指定版本：装发行版的 php-fpm / php-cli 元包，再从元包版本号反推实际版本。
    # 不能只看 `php -v`——机器上装过别的版本时，php 命令可能指向另一个版本。
    apt_install php-fpm php-cli
    ver=$(dpkg-query -W -f='${Version}' php-fpm 2>/dev/null \
      | sed -nE 's/^[0-9]+:([0-9]+\.[0-9]+).*/\1/p')
    if [[ ! "$ver" =~ ^[0-9]+\.[0-9]+$ ]]; then
      ver=$(php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null || true)
    fi
    [[ -n "$ver" ]] || die "php-fpm 装完了但判断不出版本号"
    PHP_VER="$ver"
  fi

  # 扩展一律用带版本号的包名：php-opcache 这类元包会指向最新大版本，
  # 源里同时有多个版本时（比如之前加过 Sury）会解析失败，整台机器装到一半中断
  local -a pkgs=()
  for pkg in fpm cli mysql curl gd mbstring xml zip intl bcmath opcache readline sqlite3; do
    pkg_available "php${PHP_VER}-${pkg}" && pkgs+=("php${PHP_VER}-${pkg}")
  done
  [[ ${#pkgs[@]} -gt 0 ]] || die "找不到 php${PHP_VER} 的扩展包"
  apt_install "${pkgs[@]}"

  PHP_BIN="php${PHP_VER}"
  command -v "$PHP_BIN" >/dev/null 2>&1 || PHP_BIN="php"
  PHP_SOCK="/run/php/php${PHP_VER}-fpm-wordpress.sock"
  ok "PHP 版本：${PHP_VER}（$("$PHP_BIN" -v | head -1)）"
  case "$PHP_VER" in
    8.[2-9]|9.*) ;;
    *) warn "PHP ${PHP_VER} 偏旧，WordPress 2026 年的主流插件建议 8.2 以上" ;;
  esac
}

install_web_server() {
  if [[ "$WEB_SERVER" == "apache" ]]; then
    if systemctl is-active --quiet nginx 2>/dev/null; then
      systemctl stop nginx >/dev/null 2>&1 || true
      systemctl disable nginx >/dev/null 2>&1 || true
      warn "已停用 nginx（Apache 要独占 80 端口）"
    fi
    apt_install apache2
    # mod_php 走 prefork，内存占用高；这里统一用 event + php-fpm
    a2dismod -f mpm_prefork >/dev/null 2>&1 || true
    a2dismod -f mpm_worker >/dev/null 2>&1 || true
    a2enmod -f mpm_event proxy proxy_fcgi setenvif rewrite headers expires deflate >/dev/null 2>&1 || true
    a2dissite 000-default >/dev/null 2>&1 || true
    systemctl enable apache2 >/dev/null 2>&1 || true
    ok "Apache 2.4 已安装（mpm_event + php-fpm，不装 mod_php）"
  else
    if systemctl is-active --quiet apache2 2>/dev/null; then
      systemctl stop apache2 >/dev/null 2>&1 || true
      systemctl disable apache2 >/dev/null 2>&1 || true
      warn "已停用 Apache（Nginx 要独占 80 端口）"
    fi
    apt_install nginx
    ok "Nginx 已安装"
  fi
  systemctl enable --now "$WEB_SERVICE" >/dev/null 2>&1 || true
}

section "安装 Web 服务器（${WEB_LABEL}）与 PHP"
install_web_server
install_php "$PHP_VERSION"

if [[ -n "$PHP_VERSION" ]]; then
  # 自定义版本不预装 imagick：Sury 的 php-imagick 会顺带把 php-common 升到
  # 最新版（连带 php8.5-*），为一个图片扩展塞进第二套 PHP 不划算。
  info "自定义 PHP 版本不预装 imagick，需要时再单独 apt install php${PHP_VER}-imagick"
else
  apt_install_optional php-imagick imagemagick
fi
apt_install_optional redis-server

section "安装数据库（${DB_LABEL}）"
case "$DB_ENGINE" in
  mysql84) install_mysql84 ;;
  mysql57) install_mysql57 ;;
  *)       install_mariadb ;;
esac

# 装完才有客户端，重新探测
DB_CLI=$(command -v mariadb || command -v mysql || true)
DB_DUMP=$(command -v mariadb-dump || command -v mysqldump || true)
[[ -n "$DB_CLI" ]] || die "安装后仍找不到数据库客户端（mariadb / mysql）"

systemctl enable --now "$DB_SERVICE" >/dev/null 2>&1 || true
systemctl restart "$DB_SERVICE"
ok "${WEB_LABEL} / ${DB_LABEL} 已启动"

# ---------------------------------------------------------------- PHP-FPM

section "配置 PHP-FPM（${PHP_MAX_CHILDREN} 个 worker）"

POOL_DIR="/etc/php/${PHP_VER}/fpm/pool.d"
[[ -d "$POOL_DIR" ]] || die "找不到 $POOL_DIR，PHP-FPM 未正确安装"

# 停用默认 www pool，避免两套配置抢 socket
if [[ -f "$POOL_DIR/www.conf" ]]; then
  mv "$POOL_DIR/www.conf" "$POOL_DIR/www.conf.disabled"
fi

cat > "$POOL_DIR/wordpress.conf" <<'POOL'
[wordpress]
user = www-data
group = www-data

listen = __SOCK__
listen.owner = www-data
listen.group = www-data
listen.mode = 0660

pm = ondemand
pm.max_children = __MAX_CHILDREN__
pm.process_idle_timeout = 10s
pm.max_requests = 500

; 共享池服务所有站点，chdir 只能落在 /var/www 这种必然存在的目录：
; 一旦指向某个站点目录，那个目录被删/改域名，php-fpm 会整份配置校验失败，全站 502
; 注意：pool 文件里只能用 ; 注释，# 会让 php-fpm 报 "value is NULL" 起不来
chdir = /var/www

php_admin_value[memory_limit] = __MEM_LIMIT__
php_admin_value[upload_max_filesize] = 64M
php_admin_value[post_max_size] = 64M
php_admin_value[max_execution_time] = 120
php_admin_value[max_input_time] = 120
php_admin_value[max_input_vars] = 3000
php_admin_flag[log_errors] = on
php_admin_value[error_log] = /var/log/php-fpm-wordpress.log
POOL

sed -i \
  -e "s|__SOCK__|${PHP_SOCK}|g" \
  -e "s|__MAX_CHILDREN__|${PHP_MAX_CHILDREN}|g" \
  -e "s|__ROOT__|${WEB_ROOT}|g" \
  -e "s|__MEM_LIMIT__|${PHP_MEMORY_LIMIT}|g" \
  "$POOL_DIR/wordpress.conf"

touch /var/log/php-fpm-wordpress.log
chown www-data:www-data /var/log/php-fpm-wordpress.log

# OPcache 默认只有 8M，必须调大
cat > "/etc/php/${PHP_VER}/fpm/conf.d/99-wordpress-opcache.ini" <<'OPCACHE'
opcache.enable = 1
opcache.memory_consumption = 96
opcache.interned_strings_buffer = 16
opcache.max_accelerated_files = 10000
opcache.validate_timestamps = 1
opcache.revalidate_freq = 60
opcache.save_comments = 1
OPCACHE

mkdir -p "$WEB_ROOT"
chown www-data:www-data "$WEB_ROOT"

systemctl enable php"${PHP_VER}"-fpm >/dev/null 2>&1 || true
systemctl restart php"${PHP_VER}"-fpm
ok "PHP-FPM 已重启，socket：$PHP_SOCK"

# ---------------------------------------------------------------- 数据库调优

# buffer pool 按物理内存分档：2G 给 256M，1G 给 128M，更小的机器给 96M。
# 2G 机器上 256M 是上限，再大就会把 PHP-FPM 挤进 swap。
MEM_MB=$(free -m | awk '/Mem:/{print $2}')
if   [[ "$MEM_MB" -ge 1800 ]]; then BP_MB=256
elif [[ "$MEM_MB" -ge 900  ]]; then BP_MB=128
else BP_MB=96
fi

section "配置 ${DB_LABEL}（buffer pool ${BP_MB}M）"

mkdir -p "$DB_CONF_DIR"
if [[ "$DB_ENGINE" == "mysql84" ]]; then
  # MySQL 8.4 与 MariaDB 的调优项有几处不能通用，见文件内注释
  cat > "$DB_CONF_DIR/99-wordpress.cnf" <<MYSQL
[mysqld]
# 缓冲池：最影响 WordPress 读写速度的一项
innodb_buffer_pool_size = ${BP_MB}M
# MySQL 8.0.30 起 innodb_log_file_size 被废弃，8.4 上写旧参数会直接启动失败，
# 必须换成 innodb_redo_log_capacity（默认 100M，够单站用）
innodb_redo_log_capacity = 100M
innodb_flush_method = O_DIRECT
innodb_flush_log_at_trx_commit = 2
innodb_file_per_table = 1
max_connections = 30
table_open_cache = 200
thread_cache_size = 8
tmp_table_size = 32M
max_heap_table_size = 32M
# 单机不需要 performance_schema，关掉省下上百 MB 常驻内存
performance_schema = OFF
# MySQL 8 默认开启 binlog（MariaDB 默认关闭），单机站点用不上，关掉省磁盘与写放大
skip-log-bin
# 8.4 默认不再加载 native 认证插件；老插件/老工具调用它时不会报错
mysql_native_password = ON
skip-name-resolve
character-set-server = utf8mb4
collation-server = utf8mb4_unicode_ci

[client]
default-character-set = utf8mb4
MYSQL
elif [[ "$DB_ENGINE" == "mysql57" ]]; then
  # 5.7 没有 innodb_redo_log_capacity，也没有 8.4 的认证插件开关；
  # 写错一项 mysqld 就直接起不来，所以单独一份
  cat > "$DB_CONF_DIR/99-wordpress.cnf" <<MYSQL
[mysqld]
# 缓冲池：5.7 默认 128M，低配机按内存分档压小
innodb_buffer_pool_size = ${BP_MB}M
# 5.7 仍是 innodb_log_file_size 的写法（8.0.30 才废弃）
innodb_log_file_size = 48M
innodb_flush_method = O_DIRECT
innodb_flush_log_at_trx_commit = 2
innodb_file_per_table = 1
max_connections = 30
table_open_cache = 200
thread_cache_size = 8
tmp_table_size = 32M
max_heap_table_size = 32M
# 5.7 默认开着 performance_schema，占几十 MB，单机站点关掉
performance_schema = OFF
# 5.7 默认 binlog 关闭，这里显式关掉防止镜像里打开过
skip-log-bin
skip-name-resolve
character-set-server = utf8mb4
collation-server = utf8mb4_unicode_ci

[client]
default-character-set = utf8mb4
MYSQL
else
  cat > "$DB_CONF_DIR/99-wordpress.cnf" <<MYSQL
[mysqld]
# 缓冲池：最影响 WordPress 读写速度的一项
innodb_buffer_pool_size = ${BP_MB}M
# MariaDB 仍在使用 innodb_log_file_size（MySQL 8 已改为 innodb_redo_log_capacity）
innodb_log_file_size = 64M
innodb_flush_method = O_DIRECT
innodb_flush_log_at_trx_commit = 2
innodb_file_per_table = 1
max_connections = 30
table_open_cache = 200
thread_cache_size = 8
tmp_table_size = 32M
max_heap_table_size = 32M
# 关掉 performance_schema，2G 机器上值得
performance_schema = OFF
skip-name-resolve
character-set-server = utf8mb4
collation-server = utf8mb4_unicode_ci

[client]
default-character-set = utf8mb4
MYSQL
fi

systemctl restart "$DB_SERVICE"

# 冷启动时 socket 比 systemd 的 active 状态晚几秒才可连
DB_READY=0
for _ in $(seq 1 30); do
  if "$DB_CLI" -e 'SELECT 1' >/dev/null 2>&1; then DB_READY=1; break; fi
  sleep 1
done
[[ "$DB_READY" == "1" ]] || die "${DB_LABEL} 启动后无法连接
    客户端：${DB_CLI}（$("$DB_CLI" --version 2>/dev/null | head -1)）
    排查：journalctl -u ${DB_SERVICE} -n 50
    若是 socket 路径不对，多半有残留的旧 mysql 客户端抢占了 ${DB_CLI}"

# MySQL 8 的 root 一般是 auth_socket，但个别镜像会设随机密码。
# 连不上就退到 debian.cnf 里的维护账号，保证后面的建库能继续。
DB_ADMIN_OPTS=()
if ! "$DB_CLI" -e 'SELECT 1' >/dev/null 2>&1; then
  if [[ -f /etc/mysql/debian.cnf ]] && "$DB_CLI" --defaults-file=/etc/mysql/debian.cnf -e 'SELECT 1' >/dev/null 2>&1; then
    DB_ADMIN_OPTS=(--defaults-file=/etc/mysql/debian.cnf)
    warn "root 免密登录不可用，改用 debian.cnf 维护账号"
  else
    die "无法以 root 身份连接 ${DB_LABEL}，检查 journalctl -u ${DB_SERVICE} -n 50"
  fi
fi
db_admin() { "$DB_CLI" "${DB_ADMIN_OPTS[@]}" "$@"; }

# 两道护栏，防止把已有站点踩掉：
#   1. 目标库已有表 → 那是别的站点在用，停手；
#   2. 目标账号已存在 → 不改它的密码（一改，用这个账号的站点立刻连不上库）。
EXIST_TABLES=$(db_admin -N -B -e "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='${DB_NAME}'" 2>/dev/null || echo 0)
if [[ "${EXIST_TABLES:-0}" -gt 0 ]]; then
  die "数据库 ${DB_NAME} 已存在且包含 ${EXIST_TABLES} 张表，很可能是别的站点在用。
    换个域名，或用 DB_NAME=新库名 指定一个空库。两个站共用一个库会把数据混在一起。"
fi
EXIST_USER=$(db_admin -N -B -e "SELECT COUNT(*) FROM mysql.user WHERE user='${DB_USER}' AND host='localhost'" 2>/dev/null || echo 0)
if [[ "${EXIST_USER:-0}" != "0" ]]; then
  die "数据库账号 ${DB_USER} 已存在。为避免改掉别的站点正在用的密码，这里不再继续。
    用 DB_USER=新账号名 指定一个账号，或换个域名。"
fi

db_admin -e "CREATE DATABASE IF NOT EXISTS \`${DB_NAME}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;" \
  || die "创建数据库失败，检查 ${DB_LABEL} 是否正常运行"
db_admin -e "CREATE USER '${DB_USER}'@'localhost' IDENTIFIED BY '${DB_PASS}';" \
  || die "创建数据库账号 ${DB_USER} 失败"
db_admin -e "GRANT ALL PRIVILEGES ON \`${DB_NAME}\`.* TO '${DB_USER}'@'localhost'; FLUSH PRIVILEGES;" \
  || die "授权失败：${DB_USER} → ${DB_NAME}"
DB_FULL_VERSION=$(db_admin -N -B -e 'SELECT VERSION()' 2>/dev/null | head -1)
ok "数据库 ${DB_NAME} / 用户 ${DB_USER} 已就绪（${DB_LABEL} ${DB_FULL_VERSION}）"

ok "当前内存占用：$(free -m | awk '/Mem:/{print $3" MB used / "$2" MB total"}')"

# ---------------------------------------------------------------- 站点配置

if [[ "$WEB_SERVER" == "apache" ]]; then
  section "配置 Apache 站点"

  APACHE_CONF="/etc/apache2/sites-available/${DOMAIN}.conf"
  cat > "$APACHE_CONF" <<'APACHE'
<VirtualHost *:80>
    ServerName __DOMAIN__
    ServerAlias www.__DOMAIN__
    DocumentRoot __ROOT__

    <Directory __ROOT__>
        Options -Indexes +FollowSymLinks
        # WordPress 的固定链接要靠 .htaccess，必须允许覆盖
        AllowOverride All
        Require all granted
    </Directory>

    DirectoryIndex index.php index.html

    # PHP 交给 php-fpm 的 unix socket（不用 mod_php，省内存）
    <FilesMatch "\.php$">
        SetHandler "proxy:unix:__SOCK__|fcgi://localhost"
    </FilesMatch>

    <FilesMatch "^(wp-config\.php|readme\.html)$">
        Require all denied
    </FilesMatch>

    <FilesMatch "^\.ht">
        Require all denied
    </FilesMatch>

    ErrorLog  /var/log/apache2/__DOMAIN__.error.log
    CustomLog /var/log/apache2/__DOMAIN__.access.log combined
</VirtualHost>
APACHE

  sed -i \
    -e "s|__DOMAIN__|${DOMAIN}|g" \
    -e "s|__ROOT__|${WEB_ROOT}|g" \
    -e "s|__SOCK__|${PHP_SOCK}|g" \
    "$APACHE_CONF"

  # 默认的 000-default 会抢 80 端口
  a2dissite 000-default >/dev/null 2>&1 || true
  a2ensite "${DOMAIN}" >/dev/null 2>&1 || die "启用 Apache 站点失败：a2ensite ${DOMAIN}"

  if ! APACHE_TEST=$(apache2ctl configtest 2>&1); then
    echo "$APACHE_TEST" | sed 's/^/    /' >&2
    die "Apache 配置校验失败，已中止（配置留在 ${APACHE_CONF} 便于排查）"
  fi

  systemctl enable apache2 >/dev/null 2>&1 || true
  systemctl reload apache2 2>/dev/null || systemctl restart apache2 \
    || die "Apache 启动失败。若 80 端口被其它程序占用：ss -lntp | grep ':80 '"
  SITE_CONF="$APACHE_CONF"
  ok "站点已启用：${APACHE_CONF}"
else

section "配置 Nginx 站点"

NGINX_CONF="/etc/nginx/sites-available/${DOMAIN}"
cat > "$NGINX_CONF" <<'NGINX'
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
  -e "s|__DOMAIN__|${DOMAIN}|g" \
  -e "s|__ROOT__|${WEB_ROOT}|g" \
  -e "s|__SOCK__|${PHP_SOCK}|g" \
  "$NGINX_CONF"

ln -sf "$NGINX_CONF" "/etc/nginx/sites-enabled/${DOMAIN}"
rm -f /etc/nginx/sites-enabled/default

nginx -t || die "Nginx 配置校验失败，请查看上方报错"

# nginx 可能不是 systemd 拉起来的（手工启动过、或镜像里带了常驻进程），
# 这时 systemctl reload 会报 "Unit cannot be reloaded because it is inactive"。
# 直接给进程发 reload 同样能让新配置生效，顺带把 systemd 的状态理顺。
if systemctl is-active --quiet nginx; then
  systemctl reload nginx
elif pgrep -x nginx >/dev/null 2>&1; then
  warn "Nginx 并非由 systemd 管理（进程在跑但服务是 inactive），改为直接 reload 进程"
  nginx -s reload || die "Nginx reload 失败，检查 nginx -t 与 /var/log/nginx/error.log"
  systemctl start nginx >/dev/null 2>&1 || true
else
  die "Nginx 未运行且无法启动。若 80 端口被其它程序占用，先停掉它：ss -lntp | grep ':80 '"
fi
SITE_CONF="$NGINX_CONF"
ok "站点已启用：/etc/nginx/sites-available/${DOMAIN}"

fi

# ---------------------------------------------------------------- WordPress

section "下载并安装 WordPress"

if [[ -n "$(ls -A "$WEB_ROOT" 2>/dev/null)" ]]; then
  die "$WEB_ROOT 非空，为避免覆盖已有站点已中止。请换域名或先清空目录。"
fi

# 地址顺序由网络环境决定（国内先 cn.wordpress.org，海外先 wordpress.org）
WP_SOURCES=()
while IFS= read -r _wp_src; do WP_SOURCES+=("$_wp_src"); done < <(mirror_wp_sources "$WP_LOCALE")

WP_OK=0
for src in "${WP_SOURCES[@]}"; do
  info "下载 $src"
  if curl -fsSL --retry 2 --connect-timeout 20 -m 300 "$src" -o /tmp/wordpress.tar.gz 2>/dev/null \
     && tar -tzf /tmp/wordpress.tar.gz >/dev/null 2>&1; then
    WP_OK=1
    break
  fi
  rm -f /tmp/wordpress.tar.gz
  warn "该源不可用，尝试下一个"
done
if [[ "$WP_OK" != "1" ]]; then
  die "下载 WordPress 失败（所有源均不可用），请检查服务器外网后重试"
fi

tar -xzf /tmp/wordpress.tar.gz -C "$WEB_ROOT" --strip-components=1
rm -f /tmp/wordpress.tar.gz
if [[ ! -f "$WEB_ROOT/wp-settings.php" ]]; then
  die "解压后缺少 wp-settings.php，压缩包不完整，请重试"
fi
ok "WordPress 已解压到 $WEB_ROOT"

# wp-config.php：本地生成，避免依赖外部接口
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
define( 'WP_MEMORY_LIMIT', '__MEM_LIMIT__' );
define( 'WP_MAX_MEMORY_LIMIT', '__MEM_LIMIT__' );
define( 'AUTOMATIC_UPDATER_DISABLED', false );
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
  -e "s|__MEM_LIMIT__|${PHP_MEMORY_LIMIT}|g" \
  -e "s|__S1__|$(openssl rand -base64 48 | tr -d '\n')|" \
  -e "s|__S2__|$(openssl rand -base64 48 | tr -d '\n')|" \
  -e "s|__S3__|$(openssl rand -base64 48 | tr -d '\n')|" \
  -e "s|__S4__|$(openssl rand -base64 48 | tr -d '\n')|" \
  -e "s|__S5__|$(openssl rand -base64 48 | tr -d '\n')|" \
  -e "s|__S6__|$(openssl rand -base64 48 | tr -d '\n')|" \
  -e "s|__S7__|$(openssl rand -base64 48 | tr -d '\n')|" \
  -e "s|__S8__|$(openssl rand -base64 48 | tr -d '\n')|" \
  "$WEB_ROOT/wp-config.php"

chown -R www-data:www-data "$WEB_ROOT"
find "$WEB_ROOT" -type d -exec chmod 755 {} \;
find "$WEB_ROOT" -type f -exec chmod 644 {} \;
chmod 640 "$WEB_ROOT/wp-config.php"
ok "wp-config.php 已生成（权限 640）"

# 安装 WP-CLI（国内直连 GitHub 常失败，按镜像顺序尝试；失败则降级为浏览器安装）
wp_cli_works() {
  local bin="$1"
  [[ -s "$bin" ]] || return 1
  "$PHP_BIN" "$bin" --info --allow-root >/dev/null 2>&1
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
      if [[ "$size" -gt 1000000 ]] && "$PHP_BIN" "$tmp" --info --allow-root >/dev/null 2>&1; then
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

# wp-cli 的 shebang 固定是 #!/usr/bin/env php，直接执行会用系统默认 CLI；
# 站点跑的可能是另一个版本（如 8.3），所以显式用 $PHP_BIN 起 phar。
wp_cli() {
  runuser -u www-data -- env WP_CLI_CACHE_DIR=/tmp/wp-cli-cache "$PHP_BIN" "$WP_BIN" "$@"
}

WP_CLI_READY=0
mkdir -p /tmp/wp-cli-cache && chown www-data:www-data /tmp/wp-cli-cache
if wp_cli_works "$WP_BIN"; then
  ok "WP-CLI 已就绪：$(php "$WP_BIN" --version --allow-root 2>/dev/null)"
  WP_CLI_READY=1
elif install_wp_cli && wp_cli_works "$WP_BIN"; then
  ok "WP-CLI 已安装：$(php "$WP_BIN" --version --allow-root 2>/dev/null)"
  WP_CLI_READY=1
else
  # 关键：不要留下 0 字节的空文件，否则后续脚本会误判为"已安装"
  if [[ -e "$WP_BIN" ]] && ! wp_cli_works "$WP_BIN"; then
    rm -f "$WP_BIN"
    warn "已清理无效的 $WP_BIN"
  fi
fi

if [[ "$WP_CLI_READY" -eq 1 ]]; then
  info "初始化站点数据"
  wp_cli core install \
    --path="$WEB_ROOT" \
    --url="http://${DOMAIN}" \
    --title="$SITE_TITLE" \
    --admin_user="$ADMIN_USER" \
    --admin_password="$ADMIN_PASS" \
    --admin_email="$ADMIN_EMAIL" \
    --skip-email

  wp_cli option update permalink_structure '/%postname%/' --path="$WEB_ROOT" >/dev/null

  # Apache 靠 .htaccess 做伪静态。wp rewrite flush --hard 在 CLI 下不写文件
  # （它靠 $_SERVER['SERVER_SOFTWARE'] 判断是不是 Apache，命令行里为空），
  # 所以直接落一份标准规则，内容和 WordPress 自己生成的一致。
  if [[ "$WEB_SERVER" == "apache" ]]; then
    cat > "$WEB_ROOT/.htaccess" <<'HTACCESS'
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
    chown www-data:www-data "$WEB_ROOT/.htaccess"
    chmod 644 "$WEB_ROOT/.htaccess"
  fi

  wp_cli rewrite flush --path="$WEB_ROOT" >/dev/null

  if [[ "$WP_LOCALE" == "zh_CN" ]]; then
    wp_cli language core install zh_CN --activate --path="$WEB_ROOT" >/dev/null 2>&1 \
      || warn "中文语言包安装失败，可在后台手动切换"
  fi

  wp_cli option update timezone_string 'Asia/Shanghai' --path="$WEB_ROOT" >/dev/null 2>&1 || true

  ok "站点安装完成：$SITE_TITLE"
else
  warn "WP-CLI 安装失败，已跳过自动初始化"
  warn "请浏览器访问 http://${DOMAIN}/wp-admin/install.php 完成最后一步"
fi

chown -R www-data:www-data "$WEB_ROOT"

# ---------------------------------------------------------------- Redis

if [[ "$INSTALL_REDIS" == "1" ]] && command -v redis-server >/dev/null 2>&1; then
  section "配置 Redis（对象缓存）"
  REDIS_VER=$(redis-server --version 2>/dev/null | awk '{print $3}' | sed 's/^v=//')
  if [[ -n "$REDIS_VER" ]] && dpkg --compare-versions "$REDIS_VER" lt "8.0.4" 2>/dev/null; then
    warn "Redis ${REDIS_VER} 低于 8.0.4：发行版一般已回溯修补 CVE-2025-49844 / CVE-2025-32023，"
    warn "但云安全扫描只认版本号，仍会报漏洞。建议改用 8.0.4+ 或直接不装。"
  fi
  REDIS_CONF="/etc/redis/redis.conf"
  cp -a "$REDIS_CONF" "${REDIS_CONF}.bak.$(date +%s)"
  # 追加在文件末尾，后出现的 maxmemory 覆盖发行版默认值
  {
    echo ""
    echo "# --- added by wordpress deploy script ---"
    echo "maxmemory 80mb"
    echo "maxmemory-policy allkeys-lru"
    echo "save \"\""
  } >> "$REDIS_CONF"
  systemctl enable redis-server >/dev/null 2>&1 || true
  if systemctl restart redis-server; then
    ok "Redis 已启动（maxmemory 80mb，默认仅监听 127.0.0.1）"
    info "后台安装 Redis Object Cache 插件后点 Enable 即可生效"
  else
    warn "Redis 启动失败，已可从 ${REDIS_CONF}.bak.* 恢复配置"
  fi
fi

# ---------------------------------------------------------------- 安全加固

section "安全加固"

apt_install fail2ban

cat > /etc/fail2ban/jail.local <<'JAIL'
[DEFAULT]
backend = systemd
bantime = 1h
findtime = 10m
maxretry = 5
ignoreip = 127.0.0.1/8

[sshd]
enabled = true
JAIL
systemctl enable --now fail2ban >/dev/null 2>&1 || true
if systemctl restart fail2ban; then
  ok "fail2ban 已启动（SSH 5 次失败封 1 小时）"
else
  warn "fail2ban 启动失败，可用 journalctl -u fail2ban 排查"
fi

if [[ "$ENABLE_UFW" == "1" ]]; then
  apt_install ufw
  ufw allow "${SSH_PORT}/tcp" >/dev/null
  ufw allow 80/tcp >/dev/null
  ufw allow 443/tcp >/dev/null
  ufw --force enable >/dev/null
  ok "ufw 已启用：放行 ${SSH_PORT}(SSH)、80、443"
fi

apt_install_optional unattended-upgrades
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'UPG'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
UPG
ok "自动安全更新已开启（不自动重启）"

# ---------------------------------------------------------------- HTTPS

SSL_STATUS="未启用"
if [[ "$ENABLE_SSL" == "1" ]]; then
  section "申请 HTTPS 证书"
  if [[ "$WEB_SERVER" == "apache" ]]; then
    apt_install_optional certbot python3-certbot-apache
    CERT_PLUGIN="--apache"
  else
    apt_install_optional certbot python3-certbot-nginx
    CERT_PLUGIN="--nginx"
  fi
  if command -v certbot >/dev/null 2>&1; then
    CERT_ARGS=(-d "$DOMAIN")
    if getent hosts "www.$DOMAIN" >/dev/null 2>&1; then
      CERT_ARGS+=(-d "www.$DOMAIN")
    else
      warn "www.$DOMAIN 没有解析记录，证书只申请主域名"
    fi
    if certbot "$CERT_PLUGIN" "${CERT_ARGS[@]}" \
        --non-interactive --agree-tos --redirect \
        --email "$ADMIN_EMAIL" 2>/dev/null; then
      SSL_STATUS="已启用（自动续期）"
      ok "HTTPS 已启用"
      if [[ "$WP_CLI_READY" -eq 1 ]]; then
        wp_cli option update siteurl "https://${DOMAIN}" --path="$WEB_ROOT" >/dev/null 2>&1 || true
        wp_cli option update home "https://${DOMAIN}" --path="$WEB_ROOT" >/dev/null 2>&1 || true
      fi
    else
      warn "证书申请失败，常见原因：域名未解析、安全组未放行 80/443、DNS 未生效"
      warn "稍后手动执行：certbot ${CERT_PLUGIN} -d ${DOMAIN}"
      SSL_STATUS="未启用（需手动执行 certbot）"
    fi
  else
    warn "certbot 安装失败，跳过 HTTPS"
    SSL_STATUS="未启用（certbot 未安装）"
  fi
fi

# ---------------------------------------------------------------- 日志与自检

cat > /etc/logrotate.d/php-fpm-wordpress <<'LOGROTATE'
/var/log/php-fpm-wordpress.log {
    weekly
    rotate 4
    missingok
    notifempty
    compress
    delaycompress
    copytruncate
}
LOGROTATE

section "部署自检"

if [[ "$WEB_SERVER" == "apache" ]]; then
  WEB_LOG_DIR=/var/log/apache2
else
  WEB_LOG_DIR=/var/log/nginx
fi

HTTP_CODE=$(curl -s -o /dev/null -m 15 -w '%{http_code}' -H "Host: ${DOMAIN}" http://127.0.0.1/) || HTTP_CODE="000"
case "$HTTP_CODE" in
  200|301|302) ok "本地访问返回 ${HTTP_CODE}，站点已正常响应" ;;
  000)         warn "本地访问无响应，检查：systemctl status ${WEB_SERVICE} php${PHP_VER}-fpm" ;;
  *)           warn "本地访问返回 ${HTTP_CODE}，请查看 ${WEB_LOG_DIR}/${DOMAIN}.error.log 与 /var/log/php-fpm-wordpress.log" ;;
esac

# php-fpm 版本与站点 socket 对不上时，页面会 502 且日志里只有一行 fastcgi 错误
if ! systemctl is-active --quiet "php${PHP_VER}-fpm"; then
  warn "php${PHP_VER}-fpm 没有在运行，站点会 502：systemctl status php${PHP_VER}-fpm"
fi

# ---------------------------------------------------------------- 收尾

section "部署完成"

if [[ "$SSL_STATUS" == 已启用* ]]; then
  SITE_URL="https://${DOMAIN}"
else
  SITE_URL="http://${DOMAIN}"
fi

cat > "$INFO_FILE" <<INFO_EOF
WordPress 部署信息
生成时间：$(date '+%Y-%m-%d %H:%M:%S')
------------------------------------------------------------
站点地址：   ${SITE_URL}
后台地址：   ${SITE_URL}/wp-admin/
后台账号：   ${ADMIN_USER}
后台密码：   ${ADMIN_PASS}

数据库名：   ${DB_NAME}
数据库用户： ${DB_USER}
数据库密码： ${DB_PASS}
数据库主机： localhost

网站目录：   ${WEB_ROOT}
Web 服务器： ${WEB_LABEL}
站点配置：   ${SITE_CONF}
PHP-FPM：    /etc/php/${PHP_VER}/fpm/pool.d/wordpress.conf
PHP 版本：   ${PHP_VER}
数据库：     ${DB_LABEL} ${DB_FULL_VERSION}
数据库配置： ${DB_CONF_DIR}/99-wordpress.cnf
数据库服务： ${DB_SERVICE}.service
部署日志：   ${LOG}

HTTPS：      ${SSL_STATUS}
------------------------------------------------------------
此文件含明文密码，权限已设为 600，请妥善保管并尽快删除。
INFO_EOF
chmod 600 "$INFO_FILE"

free -m | awk 'NR==1 || /Mem:|Swap:/'
echo ""
ok "后台账号：${ADMIN_USER}  密码：${ADMIN_PASS}"
ok "详细信息已保存到 ${INFO_FILE}（600 权限）"
echo ""
info "还需要你做三件事："
echo "   1. 云厂商控制台 → 安全组，确认放行 80、443（${SSH_PORT} 端口建议只放行你的 IP）"
echo "   2. 安装缓存插件（LiteSpeed Cache / FlyingPress / WP Super Cache），2G 机器靠它扛流量"
echo "   3. 配置 OSS 备份，不要把备份留在这台机器上"
echo ""
if [[ -f /var/run/reboot-required ]]; then
  warn "系统升级涉及内核，建议找时间执行 reboot 后再长期使用"
fi
info "常用命令："
echo "   systemctl status ${WEB_SERVICE} php${PHP_VER}-fpm ${DB_SERVICE}"
echo "   tail -f ${WEB_LOG_DIR}/${DOMAIN}.error.log"
echo "   tail -f /var/log/php-fpm-wordpress.log"
echo ""
