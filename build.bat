@echo off
setlocal
cd /d "%~dp0"

where go >nul 2>nul
if errorlevel 1 (
  if exist "%USERPROFILE%\sdk\go\bin\go.exe" (
    set "GO=%USERPROFILE%\sdk\go\bin\go.exe"
  ) else (
    echo [x] 没找到 go，请先安装 Go 1.21+ 或把 go.exe 加入 PATH
    exit /b 1
  )
) else (
  set "GO=go"
)

echo [*] 编译 wp-panel.exe（GUI 版，双击不弹黑窗）...
"%GO%" build -trimpath -ldflags "-s -w -H=windowsgui" -o wp-panel.exe .
if errorlevel 1 (
  echo [x] 编译失败
  exit /b 1
)

echo [*] 编译 wp-panel-console.exe（带控制台，排查问题用）...
"%GO%" build -trimpath -ldflags "-s -w" -o wp-panel-console.exe .
if errorlevel 1 (
  echo [x] 编译失败
  exit /b 1
)

echo [ok] 产物：
echo      %~dp0wp-panel.exe           双击启动，自动打开浏览器
echo      %~dp0wp-panel-console.exe   带日志窗口，启动失败时用它看原因
endlocal
