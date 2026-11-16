#!/usr/bin/env bash
# WPQU panel — 定时任务（root crontab）
# CRON_ACTION=list|add|delete|toggle|run
#
# 面板新建的条目会带一行标记注释：  # wpqu:<id> <备注>
# 停用的条目在命令前加              # wpqu-off
# 这两行都由面板写、面板读，不碰用户手写的其它行。

set -uo pipefail
export LC_ALL=C

CRON_ACTION="${CRON_ACTION:-list}"
CRON_ID="${CRON_ID:-}"
CRON_LINE="${CRON_LINE:-}"
CRON_SCHEDULE="${CRON_SCHEDULE:-}"
CRON_COMMAND="${CRON_COMMAND:-}"
CRON_LABEL="${CRON_LABEL:-}"
CRON_ENABLED="${CRON_ENABLED:-1}"

LOG_FILE=/var/log/wp-maint.log

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

need_cron() {
  command -v crontab >/dev/null 2>&1 || die "系统里没有 crontab，先执行：apt-get install -y cron"
}

# ---------------------------------------------------------------- 列表

cron_service() {
  local s
  for s in cron crond; do
    if systemctl is-active --quiet "$s" 2>/dev/null; then printf 'running'; return 0; fi
  done
  # 同样别用 systemctl | grep -q：pipefail 下 grep 提前退出会让管道判成失败
  local units
  units=$(systemctl list-unit-files 2>/dev/null)
  case "$units" in
    *"cron.service"*|*"crond.service"*) printf 'stopped' ;;
    *) printf 'none' ;;
  esac
}

emit_entries() {
  # pending_* 必须显式初始化：local 只声明不赋值，配合 set -u 一读就炸
  local tmp="$1" n=0 line trimmed entry_line enabled marker
  local pending_id="" pending_label=""
  local sched rest f1 f2 f3 f4 f5 cmd total=0

  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))
    trimmed="${line#"${line%%[![:space:]]*}"}"
    trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
    [ -n "$trimmed" ] || continue
    enabled=1

    case "$trimmed" in
      '# wpqu-off '*)
        enabled=0
        entry_line="${trimmed:11}"
        ;;
      '# wpqu:'*)
        marker="${trimmed:7}"
        marker="${marker# }"
        pending_id="${marker%% *}"
        case "$marker" in
          *' '*) pending_label="${marker#* }" ;;
          *)     pending_label="" ;;
        esac
        continue
        ;;
      '#'*) continue ;;
      *)
        # 环境变量行（PATH=... / MAILTO=...）不是任务
        if [[ "$trimmed" =~ ^[A-Za-z_][A-Za-z0-9_]*[[:space:]]*= ]]; then
          continue
        fi
        entry_line="$trimmed"
        ;;
    esac

    if [[ "$entry_line" == @* ]]; then
      sched="${entry_line%%[[:space:]]*}"
      rest="${entry_line#"$sched"}"
    else
      read -r f1 f2 f3 f4 f5 rest <<< "$entry_line"
      sched="${f1:-} ${f2:-} ${f3:-} ${f4:-} ${f5:-}"
      rest="${rest:-}"
    fi
    cmd="${rest#"${rest%%[![:space:]]*}"}"
    cmd="${cmd%"${cmd##*[![:space:]]}"}"

    [ -n "$cmd" ] || continue
    total=$((total + 1))
    local managed=0
    [ -n "$pending_id" ] && managed=1
    printf 'PANEL_CRON_ENTRY=%s|%s|%s|%s|%s|%s|%s\n' \
      "$n" "$enabled" "$managed" "${pending_id:-}" \
      "$(b64 "$sched")" "$(b64 "$cmd")" "$(b64 "$pending_label")"
    pending_id=""
    pending_label=""
  done < "$tmp"
  printf 'PANEL_CRON_TOTAL=%s\n' "$total"
}

do_list() {
  printf 'PANEL_CRON_SVC=%s\n' "$(cron_service)"
  if ! command -v crontab >/dev/null 2>&1; then
    printf 'PANEL_CRON_AVAIL=0\nPANEL_CRON_TOTAL=0\n'
    return 0
  fi
  printf 'PANEL_CRON_AVAIL=1\n'

  local tmp
  tmp=$(mktemp)
  crontab -l > "$tmp" 2>/dev/null || : > "$tmp"
  emit_entries "$tmp"
  rm -f "$tmp"
}

# ---------------------------------------------------------------- 校验

validate_schedule() {
  local s="$1"
  [ -n "$s" ] || die "执行周期不能为空"

  if [[ "$s" == @* ]]; then
    case "$s" in
      @reboot|@yearly|@annually|@monthly|@weekly|@daily|@midnight|@hourly) return 0 ;;
      *) die "不认识的周期：${s}（只支持 @reboot/@daily/@weekly/@monthly/@hourly 等）" ;;
    esac
  fi

  local -a f
  read -r -a f <<< "$s"
  [ "${#f[@]}" -eq 5 ] || die "周期要写 5 段：分 时 日 月 周（例如 0 3 * * *）"
  local x
  for x in "${f[@]}"; do
    [[ "$x" =~ ^[0-9*/,-]+$ ]] || die "周期里的「${x}」不是合法写法"
  done
}

validate_command() {
  local c="$1"
  [ -n "$c" ] || die "命令不能为空"
  [ "${#c}" -le 1000 ] || die "命令太长（最多 1000 字）"
  case "$c" in
    *$'\n'*) die "命令里不能有换行" ;;
  esac
}

# ---------------------------------------------------------------- 新增

do_add() {
  need_cron
  validate_schedule "$CRON_SCHEDULE"
  validate_command "$CRON_COMMAND"
  # 备注写在注释行里，带上换行就等于往 crontab 里插了新行
  CRON_LABEL=$(printf '%s' "$CRON_LABEL" | tr -d '\r\n' | cut -c1-60)

  local id tmp
  id=$(date +%s)
  tmp=$(mktemp)
  crontab -l > "$tmp" 2>/dev/null || : > "$tmp"
  # 末尾没有换行时直接追加会把新行接到最后一行后面
  if [ -s "$tmp" ] && [ -n "$(tail -c1 "$tmp")" ]; then printf '\n' >> "$tmp"; fi

  step "新增定时任务"
  if [ -n "$CRON_LABEL" ]; then
    printf '# wpqu:%s %s\n' "$id" "$CRON_LABEL" >> "$tmp"
  else
    printf '# wpqu:%s\n' "$id" >> "$tmp"
  fi
  printf '%s %s\n' "$CRON_SCHEDULE" "$CRON_COMMAND" >> "$tmp"

  crontab "$tmp" || { rm -f "$tmp"; die "写入 crontab 失败"; }
  rm -f "$tmp"
  ok "已添加：${CRON_SCHEDULE} ${CRON_COMMAND}"

  # 服务没跑的话任务不会执行，顺手拉起来
  if [ "$(cron_service)" = "stopped" ]; then
    systemctl start cron 2>/dev/null || systemctl start crond 2>/dev/null || true
    [ "$(cron_service)" = "running" ] && ok "cron 服务已启动"
  fi
  do_list
}

# ---------------------------------------------------------------- 删除

do_delete() {
  need_cron
  local tmp out line skip=0 n=0
  tmp=$(mktemp); out=$(mktemp)
  crontab -l > "$tmp" 2>/dev/null || : > "$tmp"

  step "删除定时任务"
  if [ -n "$CRON_ID" ]; then
    while IFS= read -r line || [ -n "$line" ]; do
      if [ "$skip" = "1" ]; then skip=0; continue; fi
      case "$line" in
        "# wpqu:${CRON_ID}"|"# wpqu:${CRON_ID} "*) skip=1; continue ;;
      esac
      printf '%s\n' "$line" >> "$out"
    done < "$tmp"
  elif [ -n "$CRON_LINE" ]; then
    awk -v n="$CRON_LINE" 'NR != n' "$tmp" > "$out"
  else
    rm -f "$tmp" "$out"
    die "缺少要删除的条目"
  fi

  crontab "$out" || { rm -f "$tmp" "$out"; die "写入 crontab 失败"; }
  rm -f "$tmp" "$out"
  ok "已删除"
  do_list
}

# ---------------------------------------------------------------- 启停

do_toggle() {
  need_cron
  [[ "$CRON_LINE" =~ ^[0-9]+$ ]] || die "缺少行号"
  local tmp out
  tmp=$(mktemp); out=$(mktemp)
  crontab -l > "$tmp" 2>/dev/null || : > "$tmp"

  awk -v n="$CRON_LINE" -v en="$CRON_ENABLED" '
    NR == n {
      if (en == "1") {
        if ($0 ~ /^# wpqu-off /) sub(/^# wpqu-off /, "")
        print; next
      }
      if ($0 !~ /^[[:space:]]*#/) print "# wpqu-off " $0
      else print
      next
    }
    { print }' "$tmp" > "$out"

  # 行号必须真指向一条任务，否则重写一遍 crontab 只会白折腾
  local target
  target=$(awk -v n="$CRON_LINE" 'NR == n { print }' "$tmp")
  [ -n "$target" ] || { rm -f "$tmp" "$out"; die "第 ${CRON_LINE} 行不存在，列表可能已变化，刷新后再试"; }
  case "$target" in
    '# wpqu-off '*) : ;;
    '#'*|'') rm -f "$tmp" "$out"; die "第 ${CRON_LINE} 行不是可启停的任务" ;;
  esac

  crontab "$out" || { rm -f "$tmp" "$out"; die "写入 crontab 失败"; }
  rm -f "$tmp" "$out"
  if [ "$CRON_ENABLED" = "1" ]; then ok "已启用"; else ok "已停用"; fi
  do_list
}

# ---------------------------------------------------------------- 立即执行

do_run() {
  validate_command "$CRON_COMMAND"
  step "立即执行"
  printf '  %s\n\n' "$CRON_COMMAND"

  local code=0
  if command -v timeout >/dev/null 2>&1; then
    timeout 180 bash -lc "$CRON_COMMAND" 2>&1 || code=$?
  else
    bash -lc "$CRON_COMMAND" 2>&1 || code=$?
  fi
  if [ "$code" -eq 124 ]; then
    die "执行超时（180 秒）"
  fi
  [ "$code" -eq 0 ] || die "退出码 ${code}"
  ok "执行完成（退出码 0）"
}

# ---------------------------------------------------------------- 入口

case "$CRON_ACTION" in
  list)   do_list ;;
  add)    begin_log; do_add ;;
  delete) begin_log; do_delete ;;
  toggle) begin_log; do_toggle ;;
  run)    begin_log; do_run ;;
  *)      die "未知操作：${CRON_ACTION}" ;;
esac
