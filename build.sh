#!/usr/bin/env bash
# 构建 Xime Clip Sync（仅 Windows amd64）
#
# 只产出一个 exe，输出到 build/ 目录，源码目录保持干净。
# 需要 Go 1.23+。如果 PATH 里的 go 不可用（例如 mise/asdf 的 shim 没生效），
# 可以用环境变量指定：GO=/path/to/go ./build.sh
set -euo pipefail

cd "$(dirname "$0")"

# ---- 解析 go 可执行文件 ----
GO_BIN="${GO:-go}"
if ! "$GO_BIN" version >/dev/null 2>&1; then
  echo "错误：找不到可用的 go（试过：$GO_BIN）" >&2
  echo "请安装 Go 1.23+，或用 GO=/path/to/go 指定路径。" >&2
  exit 1
fi
echo "==> 使用 $("$GO_BIN" version)"

export CGO_ENABLED=0
export GOOS=windows
export GOARCH=amd64
LDFLAGS="-s -w"

# 产物目录。程序运行时的 config.json / logs/ / xime-clip-sync.ui 也会落在 exe 旁边，
# 也就是 build/ 里，整个目录都在 .gitignore 中。
OUT_DIR=build
mkdir -p "$OUT_DIR"

echo "==> 运行测试"
"$GO_BIN" test ./...

# 生成图标资源：把 img/logo.ico 转成 rsrc_windows_amd64.syso
# .syso 必须留在源码根（包目录），go build 才会自动把它链接进 Windows 二进制。
# genico.go 带 //go:build ignore，不属于应用本身，所以必须显式列出参与编译的文件；
# icon.go 里放着内嵌的 logo.ico 与 ICO 解析（运行期托盘图标也要用）。
echo "==> 生成图标资源（来源 img/logo.ico）"
"$GO_BIN" run genico.go icon.go

echo "==> 构建 $OUT_DIR/xime-clip-sync.exe（无控制台窗口，常驻托盘）"
"$GO_BIN" build -trimpath -ldflags "$LDFLAGS -H=windowsgui" -o "$OUT_DIR/xime-clip-sync.exe" .

echo "==> 完成"
ls -lh "$OUT_DIR/xime-clip-sync.exe"
