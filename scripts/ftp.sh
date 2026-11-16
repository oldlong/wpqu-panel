#!/usr/bin/env bash
# WPQU panel · FTP 组件（vsftpd）按需管理
#
# 由面板上传后执行，靠环境变量传参：
#   FTP_ACTION   status | install | uninstall | start | stop | restart | config
#                | adduser | deluser | setdir | setpass | setrw
#   FTP_USER / FTP_PASS_B64 / FTP_DIR / FTP_RW / FTP_PASV_MIN / FTP_PASV_MAX / FTP_PASV_ADDR
#
# 所有机器可读的结果都用 PANEL_ 前缀单独成行，其余是给人看的日志。
set -o pipefail
export LC_ALL=C
export DEBIAN_FRONTEND=noninteractive

ACTION="${FTP_ACTION:-status}"
CONF=/etc/vsftpd.conf
UCDIR=/etc/vsftpd/user_conf
MARK=wpqu-ftp
SVC=vsftpd

PASV_MIN="${FTP_PASV_MIN:-40000}"
PASV_MAX="${FTP_PASV_MAX:-40100}"
PASV_ADDR="${FTP_PASV_ADDR:-}"

say() { printf '%s\n' "$*"; }
ok() { printf '[ok] %s\n' "$*"; }
warn() { printf '[warn] %s\n' "$*"; }
die() {
  printf '[err] %s\n' "$*" >&2
  exit 1
}

is_installed() { [ -x /usr/sbin/vsftpd ] || command -v vsftpd >/dev/null 2>&1; }
is_running() { systemctl is-active --quiet "$SVC" 2>/dev/null; }

conf_get() { awk -F= -v k="$1" '$1==k {sub(/^[^=]*=/,""); print; exit}' "$CONF" 2>/dev/null; }

# 公网地址：多个回显服务轮询，都失败就退回本机地址，但私有地址不当作公网地址返回
public_ip() {
  local ip="" url local_ip
  for url in https://api.ipify.org https://ifconfig.me/ip https://icanhazip.com https://ipinfo.io/ip; do
    ip=$(curl -fsS --max-time 4 "$url" 2>/dev/null | tr -d ' \r\n')
    case "$ip" in
      [0-9]*.[0-9]*.[0-9]*.[0-9]*) printf '%s' "$ip"; return 0 ;;
    esac
    ip=""
  done
  local_ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')
  case "$local_ip" in
    10.* | 192.168.* | 172.1[6-9].* | 172.2[0-9].* | 172.3[01].* | "") ;;
    *) printf '%s' "$local_ip" ;;
  esac
}

fw_state() {
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | head -1 | grep -qi active; then
    printf 'ufw'
  elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    printf 'firewalld'
  else
    printf 'none'
  fi
}

fw_open() {
  case "$(fw_state)" in
    ufw)
      ufw allow 21/tcp >/dev/null 2>&1 && ok "防火墙已放行 21/tcp"
      ufw allow "${PASV_MIN}:${PASV_MAX}/tcp" >/dev/null 2>&1 && ok "防火墙已放行被动端口 ${PASV_MIN}-${PASV_MAX}"
      ;;
    firewalld)
      firewall-cmd --permanent --add-port=21/tcp >/dev/null 2>&1
      firewall-cmd --permanent --add-port="${PASV_MIN}-${PASV_MAX}/tcp" >/dev/null 2>&1
      firewall-cmd --reload >/dev/null 2>&1 && ok "防火墙已放行 21/tcp 与被动端口"
      ;;
    *)
      warn "本机没有启用 ufw / firewalld，跳过防火墙规则"
      ;;
  esac
}

# Debian 的 /etc/pam.d/vsftpd 末尾有一行 pam_shells.so，
# 它要求账号的 shell 出现在 /etc/shells 里；面板建的是 nologin 账号，会被拒成 530。
# 只注释这一行（不动 vsftpd.conf 的 check_shell，也不改 /etc/shells）。
patch_pam() {
  local f=/etc/pam.d/vsftpd
  [ -f "$f" ] || return 0
  if grep -qE '^[[:space:]]*[^#[:space:]].*pam_shells\.so' "$f"; then
    [ -f "$f.panel-orig" ] || cp -a "$f" "$f.panel-orig" 2>/dev/null
    sed -i -E 's|^([[:space:]]*[^#[:space:]].*pam_shells\.so.*)$|# \1   # 面板：FTP 账号用 nologin，不能过 pam_shells|' "$f"
    ok "已关闭 FTP 的 pam_shells 检查（nologin 账号才能登录）"
  fi
}

write_conf() {
  if [ -f "$CONF" ] && [ ! -f "$CONF.panel-orig" ]; then
    cp -a "$CONF" "$CONF.panel-orig" 2>/dev/null && ok "原配置已备份为 ${CONF}.panel-orig"
  fi
  {
    echo "# 由 WPQU panel 生成，手工修改会在下次面板操作时被覆盖"
    echo "listen=YES"
    echo "listen_ipv6=NO"
    echo "anonymous_enable=NO"
    echo "local_enable=YES"
    echo "write_enable=YES"
    echo "local_umask=022"
    echo "dirmessage_enable=YES"
    echo "use_localtime=YES"
    echo "xferlog_enable=YES"
    echo "connect_from_port_20=NO"
    echo "chroot_local_user=YES"
    echo "allow_writeable_chroot=YES"
    echo "check_shell=NO"
    echo "pasv_enable=YES"
    echo "pasv_min_port=${PASV_MIN}"
    echo "pasv_max_port=${PASV_MAX}"
    [ -n "$PASV_ADDR" ] && echo "pasv_address=${PASV_ADDR}"
    echo "user_config_dir=${UCDIR}"
    echo "secure_chroot_dir=/var/run/vsftpd/empty"
    echo "pam_service_name=vsftpd"
    echo "seccomp_sandbox=NO"
    echo "ssl_enable=NO"
  } >"$CONF"
  mkdir -p "$UCDIR" /var/run/vsftpd/empty
  chmod 555 /var/run/vsftpd/empty 2>/dev/null
}

restart_svc() {
  systemctl enable "$SVC" >/dev/null 2>&1
  systemctl restart "$SVC" >/dev/null 2>&1
  sleep 1
  if is_running; then
    ok "vsftpd 已启动（21 端口，被动 ${PASV_MIN}-${PASV_MAX}）"
  else
    warn "vsftpd 未能启动，最近日志："
    journalctl -u "$SVC" -n 15 --no-pager 2>/dev/null | sed 's/^/  /'
    die "启动失败"
  fi
}

print_status() {
  local ip installed=0 running=0 ver="" port=21
  is_installed && installed=1
  is_running && running=1
  if [ "$installed" = 1 ]; then
    ver=$(dpkg-query -W -f='${Version}' vsftpd 2>/dev/null)
    if [ -z "$ver" ]; then ver=$("$SVC" -v 2>&1 | head -1 | sed 's/^vsftpd: *//'); fi
    [ -z "$ver" ] && ver="未知版本"
  fi
  ip=$(public_ip)

  echo "PANEL_INSTALLED=$installed"
  echo "PANEL_RUNNING=$running"
  echo "PANEL_VERSION=$ver"
  echo "PANEL_PORT=$port"
  echo "PANEL_PASV_MIN=$(conf_get pasv_min_port)"
  echo "PANEL_PASV_MAX=$(conf_get pasv_max_port)"
  echo "PANEL_PASV_ADDR=$(conf_get pasv_address)"
  echo "PANEL_PUBLIC_IP=$ip"
  echo "PANEL_FW=$(fw_state)"

  [ "$installed" = 1 ] || return 0

  getent passwd | awk -F: -v m="$MARK" '$5==m {print $1"|"$6}' | while IFS='|' read -r u home; do
    [ -z "$u" ] && continue
    rw=rw
    if [ -f "$UCDIR/$u" ] && grep -qi '^write_enable=NO' "$UCDIR/$u" 2>/dev/null; then rw=ro; fi
    st=active
    pw=$(getent shadow "$u" 2>/dev/null | cut -d: -f2)
    case "$pw" in '!'*|'*'*|'') st=locked ;; esac
    echo "PANEL_USER=${u}|${home}|${rw}|${st}"
  done
}

need_root() { [ "$(id -u)" = 0 ] || die "需要 root 权限，请用 root 账号连接服务器"; }

user_exists() { getent passwd "$1" >/dev/null 2>&1; }

valid_name() { printf '%s' "$1" | grep -Eq '^[a-z][a-z0-9_-]{2,31}$'; }

# FTP 用户以目录属主的身份读写：上传的文件 nginx / WordPress 立刻能用
owner_of() { stat -c '%u:%g' "$1" 2>/dev/null; }

# ---------------------------------------------------------------- 动作

act_install() {
  need_root
  if is_installed; then
    ok "vsftpd 已安装，跳过安装步骤"
  else
    say "› 安装 vsftpd…"
    if ! apt-get install -y --no-install-recommends vsftpd >/dev/null 2>&1; then
      say "› 软件索引可能过期，先 apt-get update 再重试…"
      apt-get update -qq >/dev/null 2>&1 || warn "apt-get update 有报错，继续尝试安装"
      apt-get install -y --no-install-recommends vsftpd >/dev/null 2>&1 || die "vsftpd 安装失败，请检查软件源"
    fi
    ok "$(dpkg-query -W -f='${Version}' vsftpd 2>/dev/null | sed 's/^/vsftpd /') 安装完成"
  fi

  # 其他 FTP 服务占着 21 端口会让 vsftpd 起不来
  if ss -ltnp 2>/dev/null | grep -q ':21 ' && ! is_running; then
    warn "21 端口已被其他进程占用："
    ss -ltnp 2>/dev/null | grep ':21 ' | sed 's/^/  /'
  fi

  if [ -z "$PASV_ADDR" ]; then
    PASV_ADDR=$(public_ip)
    if [ -n "$PASV_ADDR" ]; then
      ok "已自动识别公网地址 ${PASV_ADDR}（云主机 NAT 需要它，被动模式才连得上）"
    else
      warn "没能识别公网地址，被动模式下客户端可能连不上；请在本页「公网地址」里手工填写后保存"
    fi
  fi

  write_conf
  patch_pam
  ok "配置已写入 ${CONF}（禁止匿名、chroot 到用户目录、被动端口 ${PASV_MIN}-${PASV_MAX}）"
  fw_open
  restart_svc
  if [ -n "$PASV_ADDR" ]; then ok "被动模式公网地址：${PASV_ADDR}"; fi
  warn "云主机还需在控制台安全组放行 21 与 ${PASV_MIN}-${PASV_MAX}（TCP）"
}

act_uninstall() {
  need_root
  say "› 停止 vsftpd 并取消开机启动…"
  systemctl stop "$SVC" >/dev/null 2>&1
  systemctl disable "$SVC" >/dev/null 2>&1

  local n=0 min max
  # 配置一会儿会被删，先把端口段记下来，卸载后好回收防火墙规则
  min=$(conf_get pasv_min_port)
  max=$(conf_get pasv_max_port)
  for u in $(getent passwd | awk -F: -v m="$MARK" '$5==m {print $1}'); do
    userdel -f "$u" >/dev/null 2>&1 && n=$((n + 1))
  done
  [ "$n" -gt 0 ] && ok "已删除 ${n} 个面板创建的 FTP 账号（站点文件保留，未动）"

  say "› 卸载 vsftpd 软件包…"
  apt-get purge -y vsftpd >/dev/null 2>&1
  apt-get autoremove -y >/dev/null 2>&1
  rm -rf "$UCDIR" "$CONF.panel-orig"
  rm -f "$CONF"
  ok "组件已卸载，配置与账号目录已清理"

  if [ "$(fw_state)" = "ufw" ]; then
    ufw delete allow 21/tcp >/dev/null 2>&1
    if [ -n "$min" ] && [ -n "$max" ]; then
      ufw delete allow "${min}:${max}/tcp" >/dev/null 2>&1
    fi
    ok "防火墙里 21 与被动端口的放行规则已回收"
  fi
}

act_control() {
  need_root
  is_installed || die "FTP 组件还没安装"
  case "$ACTION" in
    start) systemctl start "$SVC" ;;
    stop) systemctl stop "$SVC" ;;
    restart) systemctl restart "$SVC" ;;
  esac
  sleep 1
  if [ "$ACTION" = "stop" ]; then
    is_running && die "停止失败，vsftpd 仍在运行" || ok "vsftpd 已停止（21 端口已关闭）"
  else
    is_running && ok "vsftpd 正在运行" || die "启动失败，看看 journalctl -u vsftpd"
  fi
}

act_config() {
  need_root
  is_installed || die "FTP 组件还没安装"
  [ "$PASV_MIN" -gt 1024 ] 2>/dev/null || die "被动端口最小值不合法"
  [ "$PASV_MAX" -lt 65535 ] 2>/dev/null || die "被动端口最大值不合法"
  [ "$PASV_MIN" -le "$PASV_MAX" ] || die "被动端口范围写反了"
  if [ -z "$PASV_ADDR" ]; then
    PASV_ADDR=$(conf_get pasv_address)
    [ -z "$PASV_ADDR" ] && PASV_ADDR=$(public_ip)
  fi
  write_conf
  ok "被动端口已改为 ${PASV_MIN}-${PASV_MAX}"
  fw_open
  restart_svc
}

# 本机回环登录一次，验证 PAM / chroot / 被动模式都通（curl 不在就跳过）
login_test() {
  local u="$1" p="$2" dir="$3" mode="$4"
  command -v curl >/dev/null 2>&1 || {
    warn "本机没有 curl，跳过登录自检"
    return 0
  }
  local out
  if out=$(curl -sS --ftp-skip-pasv-ip --max-time 12 --list-only -u "${u}:${p}" ftp://127.0.0.1/ 2>&1); then
    ok "登录自检通过（目录列表：$(printf '%s' "$out" | head -3 | tr '\n' ' ')）"
  else
    warn "登录自检未通过：${out}"
    warn "账号已创建，但请检查 /var/log/vsftpd.log 或 journalctl -u vsftpd"
    return 0
  fi

  # 可写账号再传一个探针文件，确认目录真的能写入（随后删掉）
  if [ "$mode" = "rw" ] && [ -n "$dir" ]; then
    local probe=".wpqu_probe_$$" tmpf=/tmp/.wpqu-probe-$$
    printf 'ok\n' >"$tmpf"
    if curl -sS --ftp-skip-pasv-ip --max-time 15 -T "$tmpf" -u "${u}:${p}" "ftp://127.0.0.1/${probe}" >/dev/null 2>&1; then
      ok "上传自检通过（已写入并删除探针文件）"
    else
      warn "上传自检未通过：目录对 ${u} 不可写，检查属主与权限（755/775）"
    fi
    rm -f "${dir}/${probe}" "$tmpf"
  fi
}

act_adduser() {
  need_root
  is_installed || die "FTP 组件还没安装，先安装再建账号"
  local u="${FTP_USER:-}" dir="${FTP_DIR:-}" rw="${FTP_RW:-rw}" b64="${FTP_PASS_B64:-}"
  valid_name "$u" || die "用户名只能用 3-32 位小写字母、数字、下划线、连字符，且以字母开头"
  user_exists "$u" && die "系统里已有同名账号 ${u}，换一个名字"
  case "$dir" in /?*) ;; *) die "目录必须是绝对路径" ;; esac
  [ -n "$b64" ] || die "缺少密码"

  [ -d "$dir" ] || {
    mkdir -p "$dir" || die "目录创建失败：$dir"
    ok "目录不存在，已创建 ${dir}"
  }

  local owner uid gid
  owner=$(owner_of "$dir")
  uid="${owner%%:*}"
  gid="${owner##*:}"
  if [ "$uid" = "0" ]; then
    # 属主是 root 时不映射，避免建出 uid 0 的账号
    useradd -d "$dir" -s /usr/sbin/nologin -M -c "$MARK" "$u" || die "创建账号失败"
    warn "目录属主是 root，账号使用独立 UID；上传的文件属主会是 ${u}"
  else
    if useradd -o -u "$uid" -g "$gid" -d "$dir" -s /usr/sbin/nologin -M -c "$MARK" "$u" 2>/dev/null; then
      ok "账号 UID/GID 与目录属主一致（${uid}:${gid}），上传的文件归属性正确"
    elif useradd -d "$dir" -s /usr/sbin/nologin -M -c "$MARK" "$u" 2>/dev/null; then
      warn "无法复用目录属主的 UID，账号使用独立 UID；上传的文件属主会是 ${u}"
    else
      die "创建账号失败"
    fi
  fi

  printf '%s' "$b64" | base64 -d | { IFS= read -r p || true; printf '%s:%s\n' "$u" "$p"; } | chpasswd >/dev/null 2>&1 ||
    die "设置密码失败"
  ok "密码已设置"

  patch_pam
  mkdir -p "$UCDIR"
  if [ "$rw" = "ro" ]; then
    printf 'write_enable=NO\n' >"$UCDIR/$u"
    ok "该账号为只读（能下载、不能上传/删除）"
  else
    printf 'write_enable=YES\n' >"$UCDIR/$u"
    ok "该账号为可读写"
  fi

  systemctl reload "$SVC" >/dev/null 2>&1 || systemctl restart "$SVC" >/dev/null 2>&1
  ok "FTP 账号 ${u} 已创建，根目录 ${dir}"
  login_test "$u" "$(printf '%s' "$b64" | base64 -d)" "$dir" "$rw"
}

act_deluser() {
  need_root
  local u="${FTP_USER:-}"
  valid_name "$u" || die "用户名不合法"
  user_exists "$u" || die "账号 ${u} 不存在"
  gecos=$(getent passwd "$u" | cut -d: -f5)
  [ "$gecos" = "$MARK" ] || die "${u} 不是面板创建的账号，拒绝删除"
  # -f 会跳过「用户正被使用」的检查；不能 pkill -u，同 UID 的 php-fpm 会被一起杀掉
  msg=$(userdel -f "$u" 2>&1) || die "删除账号失败：${msg}"
  rm -f "$UCDIR/$u"
  ok "账号 ${u} 已删除（目录与文件保留）"
}

# 面板的 FTP 账号复用目录属主的 UID（例如 www-data），
# 于是 usermod / userdel 会认为「用户正被 php-fpm 使用」而拒绝执行。
# 改根目录只是动 /etc/passwd 的第 6 个字段，直接改写比 usermod 可靠。
set_home_field() {
  local u="$1" d="$2" t
  t=$(mktemp) || die "mktemp 失败"
  awk -F: -v u="$u" -v d="$d" 'BEGIN{OFS=":"} {if ($1==u) $6=d; print}' /etc/passwd >"$t" ||
    { rm -f "$t"; die "改写 /etc/passwd 失败"; }
  cat "$t" >/etc/passwd
  rm -f "$t"
  [ "$(getent passwd "$u" | cut -d: -f6)" = "$d" ] || die "根目录没有改成功，请检查 /etc/passwd"
}

act_setdir() {
  need_root
  local u="${FTP_USER:-}" dir="${FTP_DIR:-}"
  valid_name "$u" || die "用户名不合法"
  user_exists "$u" || die "账号 ${u} 不存在"
  case "$dir" in /?*) ;; *) die "目录必须是绝对路径" ;; esac
  [ -d "$dir" ] || die "目录不存在：$dir"
  set_home_field "$u" "$dir"
  ok "账号 ${u} 的 FTP 根目录已改为 ${dir}（文件未移动）"
}

act_setpass() {
  need_root
  local u="${FTP_USER:-}" b64="${FTP_PASS_B64:-}"
  valid_name "$u" || die "用户名不合法"
  user_exists "$u" || die "账号 ${u} 不存在"
  [ -n "$b64" ] || die "缺少密码"
  local p
  p=$(printf '%s' "$b64" | base64 -d)
  printf '%s:%s\n' "$u" "$p" | chpasswd >/dev/null 2>&1 || die "设置密码失败"
  ok "账号 ${u} 的密码已更新"
  # 顺手验证新密码能登录（不做上传探针，改密码不该动文件）
  login_test "$u" "$p" "" ""
}

act_setrw() {
  need_root
  local u="${FTP_USER:-}" rw="${FTP_RW:-rw}"
  valid_name "$u" || die "用户名不合法"
  user_exists "$u" || die "账号 ${u} 不存在"
  mkdir -p "$UCDIR"
  if [ "$rw" = "ro" ]; then
    printf 'write_enable=NO\n' >"$UCDIR/$u"
    ok "账号 ${u} 已改为只读"
  else
    printf 'write_enable=YES\n' >"$UCDIR/$u"
    ok "账号 ${u} 已改为可读写"
  fi
  systemctl reload "$SVC" >/dev/null 2>&1 || systemctl restart "$SVC" >/dev/null 2>&1
}

case "$ACTION" in
  status) print_status ;;
  install) act_install && print_status ;;
  uninstall) act_uninstall && print_status ;;
  start | stop | restart) act_control && print_status ;;
  config) act_config && print_status ;;
  adduser) act_adduser && print_status ;;
  deluser) act_deluser && print_status ;;
  setdir) act_setdir && print_status ;;
  setpass) act_setpass ;;
  setrw) act_setrw && print_status ;;
  *) die "未知动作：${ACTION}" ;;
esac
