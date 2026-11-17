#!/usr/bin/env bash
#
# 把 _mirrors.sh 注入到各脚本的标记之间（面板脚本 + 插件安装脚本）。
#
# 和 sync-ws-lib.sh 同一个理由：面板把单个脚本上传到 /tmp 再 bash 执行，远端没有
# 库文件，插件包更是只带自己那几个文件。所以每个脚本都内嵌一份副本，靠这个工具
# 保证十几份副本完全一致。
#
# 用法：
#   bash scripts/sync-mirrors.sh          # 注入/更新（标记不存在时自动补上）
#   bash scripts/sync-mirrors.sh --check  # 只检查是否一致（提交前用）

set -euo pipefail

DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
LIB="$DIR/_mirrors.sh"
PLUGINS="$DIR/../../plugins-src"

BEGIN_MARK='# >>> mirror-lib BEGIN'
END_MARK='# <<< mirror-lib END'

CHECK=0
[[ "${1:-}" = "--check" ]] && CHECK=1

[[ -f "$LIB" ]] || { echo "找不到 $LIB" >&2; exit 1; }

# 面板脚本：会装包 / 下载东西的那几个
TARGETS=(
  "$DIR/add-site.sh"
  "$DIR/install-wordpress.sh"
  "$DIR/change-domain.sh"
  "$DIR/site-convert.sh"
  "$DIR/php-manage.sh"
  "$DIR/ftp.sh"
)
# 插件脚本：凡是带 apt / pip / npm / 下载的安装脚本
if [[ -d "$PLUGINS" ]]; then
  while IFS= read -r f; do
    TARGETS+=("$f")
  done < <(find "$PLUGINS" -mindepth 3 -maxdepth 3 -path '*/scripts/install.sh' | sort)
fi

tmp=$(mktemp "${TMPDIR:-/tmp}/mirror-lib.XXXXXX")
tmp2=$(mktemp "${TMPDIR:-/tmp}/mirror-lib2.XXXXXX")
cleanup() { rm -f "$tmp" "$tmp2" 2>/dev/null || true; }
trap cleanup EXIT

fail=0
for file in "${TARGETS[@]}"; do
  name="${file#"$DIR"/}"
  if [[ ! -f "$file" ]]; then
    echo "缺少脚本：$file" >&2
    fail=1
    continue
  fi

  # 1) 没有标记就先插一对空标记：放在第一行 set -euo pipefail 之后
  if ! grep -qxF "$BEGIN_MARK" "$file"; then
    awk -v bm="$BEGIN_MARK" -v em="$END_MARK" '
      !done && /^set -[A-Za-z]+ pipefail[[:space:]]*$/ { print; print bm; print em; done=1; next }
      !done && /^set -[A-Za-z]+[[:space:]]*$/       { print; print bm; print em; done=1; next }
      { print }
    ' "$file" >"$tmp2"
    if ! grep -qxF "$BEGIN_MARK" "$tmp2"; then
      echo "$name 里既没有标记，也找不到 set -euo pipefail，请手工插一对标记" >&2
      fail=1
      continue
    fi
    if [[ "$CHECK" == "1" ]]; then
      echo "$name 缺少 mirror-lib 标记（运行 bash scripts/sync-mirrors.sh 补上）" >&2
      fail=1
      continue
    fi
    cat "$tmp2" >"$file"
  fi

  # 2) 用库文件整体替换两个标记之间的内容（标记行本身也一起换）
  awk -v lib="$LIB" -v bm="$BEGIN_MARK" -v em="$END_MARK" '
    BEGIN {
      block = ""
      while ((getline line < lib) > 0) block = block line "\n"
      close(lib)
    }
    $0 == bm { printf "%s", block; skip = 1; next }
    $0 == em { skip = 0; next }
    skip != 1 { print }
  ' "$file" >"$tmp"

  if ! cmp -s "$file" "$tmp"; then
    if [[ "$CHECK" == "1" ]]; then
      echo "$name 的 mirror-lib 副本不是最新的" >&2
      fail=1
    else
      cat "$tmp" >"$file"
      printf 'updated %s\n' "$name"
    fi
  fi
done

if [[ "$fail" != "0" ]]; then
  exit 1
fi
if [[ "$CHECK" == "1" ]]; then
  echo "mirror-lib 注入一致（${#TARGETS[@]} 个脚本）"
else
  echo "完成：${#TARGETS[@]} 个脚本已同步"
fi
