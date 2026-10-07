@echo off
REM 构建 Xime Clip Sync（仅 Windows amd64）：桌面版（无控制台 + 托盘）
REM 只产出一个 exe，输出到 build\ 目录，源码目录保持干净。
REM 需要 Go 1.23+。如果 go 不在 PATH 中，可以设 GO=完整路径 后再运行。
setlocal
REM 本文件是 UTF-8 编码，切到 65001 才能让下面的中文提示正常显示。
chcp 65001 >nul

set GO_BIN=%GO%
if "%GO_BIN%"=="" set GO_BIN=go
%GO_BIN% version >nul 2>nul
if errorlevel 1 (
  echo [错误] 未找到可用的 go（试过：%GO_BIN%）
  echo         请安装 Go 1.23 或更高版本：https://go.dev/dl/
  echo         或用 GO=D:\path\to\go.exe 指定路径后重试。
  exit /b 1
)

set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64

REM 产物目录。程序运行时的 config.json / logs / xime-clip-sync.ui 也会落在 exe 旁边，
REM 也就是 build\ 里，整个目录都在 .gitignore 中。
set OUT_DIR=build
if not exist "%OUT_DIR%" mkdir "%OUT_DIR%"

echo ==^> 运行测试
%GO_BIN% test ./... || exit /b 1

echo ==^> 生成图标资源（来源 img/logo.ico）
REM genico.go 带 //go:build ignore，不属于应用本身，必须显式列出参与编译的文件。
REM icon.go 里放着内嵌的 logo.ico 与 ICO 解析（运行期托盘图标也要用）。
REM 产出的 rsrc_windows_amd64.syso 留在源码根，下面的 go build 会自动链接进去。
%GO_BIN% run genico.go icon.go || exit /b 1

echo ==^> 构建 %OUT_DIR%\xime-clip-sync.exe（无控制台窗口，常驻托盘）
%GO_BIN% build -trimpath -ldflags "-s -w -H=windowsgui" -o "%OUT_DIR%\xime-clip-sync.exe" . || exit /b 1

echo ==^> 完成
dir /b "%OUT_DIR%\xime-clip-sync.exe"
endlocal
