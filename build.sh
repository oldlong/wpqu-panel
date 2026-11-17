#!/usr/bin/env bash
# 编译 wpqu-panel。
#
#   ./build.sh          编译当前平台（Windows 同时产出 GUI 版与 console 版）
#   ./build.sh all      交叉编译三个平台到 ../dist/
#   ./build.sh admin    只编译管理后台 wpquadmin 到 ../dist/admin/
#
# Windows 的 GUI 版（-H=windowsgui）双击不弹黑窗，启动后自动开浏览器；
# 排查问题时用 console 版，能看到日志输出。
#
# 管理后台单独放在 dist/admin/：它不参与升级清单，混在 dist/ 里会让
# 「这次到底会发出去哪几个文件」变得不清楚。
set -euo pipefail
cd "$(dirname "$0")"

VERSION=$(grep -o 'Version = "[^"]*"' internal/httpapi/http.go | head -1 | cut -d'"' -f2)
echo "[*] wpqu-panel v${VERSION}"

build() { # $1=GOOS $2=GOARCH $3=out $4=extra ldflags
  GOOS=$1 GOARCH=$2 CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w $4" -o "$3" .
  echo "[ok] $3"
}

if [[ "${1:-}" == "admin" ]]; then
  mkdir -p ../dist/admin
  for os in linux windows darwin; do
    ext=""
    [[ "$os" == "windows" ]] && ext=".exe"
    arch="amd64"
    [[ "$os" == "darwin" ]] && arch="arm64"
    GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
      -o "../dist/admin/wpquadmin-${os}-${arch}${ext}" ./cmd/wpquadmin
    echo "[ok] ../dist/admin/wpquadmin-${os}-${arch}${ext}"
  done
  echo "[done] 管理后台产物在 $(cd ../dist/admin && pwd)"
  echo "      Windows 双击 exe 即可（首次会自动生成口令并开浏览器）"
  exit 0
fi

if [[ "${1:-}" == "all" ]]; then
  mkdir -p ../dist
  build windows amd64 ../dist/wpqu-panel-windows-amd64.exe "-H=windowsgui"
  build windows amd64 ../dist/wpqu-panel-windows-console.exe ""
  build linux   amd64 ../dist/wpqu-panel-linux-amd64 ""
  build darwin  arm64 ../dist/wpqu-panel-darwin-arm64 ""
  echo "[done] 产物在 $(cd ../dist && pwd)"
  exit 0
fi

if [[ "$(go env GOOS)" == "windows" ]]; then
  build windows "$(go env GOARCH)" wpqu-panel.exe "-H=windowsgui"
  build windows "$(go env GOARCH)" wpqu-panel-console.exe ""
else
  build "$(go env GOOS)" "$(go env GOARCH)" wpqu-panel ""
fi
