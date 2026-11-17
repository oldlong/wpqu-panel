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
