#!/usr/bin/env bash
#
# 把 _ws-lib.sh 注入到各脚本的标记之间。
#
# 为什么用注入而不是 source：
#   面板是把单个脚本上传到 /tmp 再 bash 执行的，远端没有库文件；手册里也写了
#   「可以直接把 add-site.sh 拷到服务器上跑」。所以每个脚本都带一份自己的副本，
#   由这个工具保证五份副本内容完全一致。
#
# 用法：
#   bash scripts/sync-ws-lib.sh          # 注入/更新
#   bash scripts/sync-ws-lib.sh --check  # 只检查是否一致（CI / 提交前用）

set -euo pipefail

DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
LIB="$DIR/_ws-lib.sh"
TARGETS=(add-site.sh change-domain.sh delete-site.sh site-convert.sh php-manage.sh)
BEGIN_MARK='# >>> ws-lib BEGIN'
END_MARK='# <<< ws-lib END'

CHECK=0
[[ "${1:-}" = "--check" ]] && CHECK=1

[[ -f "$LIB" ]] || { echo "找不到 $LIB" >&2; exit 1; }

tmp=$(mktemp "${TMPDIR:-/tmp}/ws-lib.XXXXXX")
cleanup() { rm -f "$tmp" 2>/dev/null || true; }
trap cleanup EXIT

fail=0
for name in "${TARGETS[@]}"; do
  file="$DIR/$name"
  if [[ ! -f "$file" ]]; then
    echo "缺少脚本：$name" >&2
    fail=1
    continue
  fi
  if ! grep -qxF "$BEGIN_MARK" "$file"; then
    echo "$name 里没有 ws-lib 标记行，请先手工插入：$BEGIN_MARK ... $END_MARK" >&2
    fail=1
    continue
  fi

  # 用库文件整体替换两个标记之间的内容（标记行本身也一起换）
  awk -v lib="$LIB" -v bm="$BEGIN_MARK" -v em="$END_MARK" '
    BEGIN {
      block = ""
      while ((getline line < lib) > 0) block = block line "\n"
      close(lib)
    }
    $0 == bm { printf "%s", block; skip = 1; next }
    $0 == em { skip = 0; next }
    skip != 1 { print }
  ' "$file" > "$tmp"

  if [[ "$CHECK" == "1" ]]; then
    if ! cmp -s "$file" "$tmp"; then
      echo "$name 的 ws-lib 副本不是最新的" >&2
      fail=1
    fi
  else
    cat "$tmp" > "$file"
    chmod 644 "$file"
    printf 'updated %s\n' "$name"
  fi
done

if [[ "$fail" != "0" ]]; then
  exit 1
fi

if [[ "$CHECK" == "1" ]]; then
  echo "ws-lib 注入一致"
fi
