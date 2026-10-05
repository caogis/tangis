#!/usr/bin/env bash
# TanGIS 桌面单机版 —— 跨平台打包脚本
#
# 产物：
#   dist/tangis-<os>-<arch>/       可直接分发的目录
#   dist/tangis-<os>-<arch>.zip    压缩包（zip 保留可执行权限）
#
# 包内含：主二进制（内嵌 Web 控制台）、对应平台的切片内核、启停脚本、README。
# 不需要代码签名/公证：用户解压后运行 start 脚本，浏览器访问固定端口即可。
#
# 用法：
#   deploy/desktop/build-dist.sh darwin  arm64    # Apple Silicon
#   deploy/desktop/build-dist.sh darwin  amd64    # Intel Mac
#   deploy/desktop/build-dist.sh windows amd64    # Windows x64（需 mingw-w64）
#   deploy/desktop/build-dist.sh linux   amd64    # Linux x64（需 musl 交叉工具链）
#
# 前置：Go 工具链、Rust（含目标 target）、前端已构建（脚本会自动补构建）。
set -euo pipefail

GOOS_T="${1:-}"
GOARCH_T="${2:-}"
if [ -z "$GOOS_T" ] || [ -z "$GOARCH_T" ]; then
  echo "usage: $0 <goos> <goarch>   e.g. $0 darwin arm64" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$ROOT/dist/tangis-$GOOS_T-$GOARCH_T"
EXE=""
[ "$GOOS_T" = "windows" ] && EXE=".exe"

# Go 目标 → Rust target triple（内核必须与主二进制同平台）
case "$GOOS_T/$GOARCH_T" in
  darwin/arm64)  TRIPLE="aarch64-apple-darwin" ;;
  darwin/amd64)  TRIPLE="x86_64-apple-darwin" ;;
  windows/amd64) TRIPLE="x86_64-pc-windows-gnu" ;;
  linux/amd64)   TRIPLE="x86_64-unknown-linux-musl" ;;
  linux/arm64)   TRIPLE="aarch64-unknown-linux-musl" ;;
  *)
    echo "unsupported target: $GOOS_T/$GOARCH_T" >&2
    echo "supported: darwin/{arm64,amd64} windows/amd64 linux/{amd64,arm64}" >&2
    exit 2
    ;;
esac

# ---- 依赖检查（宁可在构建前快速失败，也不要产出一个跑不起来的包）----
if ! command -v rustup >/dev/null 2>&1; then
  echo "未找到 rustup，无法确认 Rust 目标；请安装 Rust 工具链" >&2
  exit 3
fi
if ! rustup target list --installed 2>/dev/null | grep -qx "$TRIPLE"; then
  echo "缺少 Rust 目标 $TRIPLE，请先执行：" >&2
  echo "  rustup target add $TRIPLE" >&2
  exit 3
fi

# 交叉链接器：Windows 走 mingw，Linux 走 musl（静态，不依赖目标机 glibc）
LINKER_ENV=""
case "$TRIPLE" in
  x86_64-pc-windows-gnu)
    if ! command -v x86_64-w64-mingw32-gcc >/dev/null 2>&1; then
      echo "缺少 mingw-w64：brew install mingw-w64" >&2
      exit 3
    fi
    LINKER_ENV="CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER=x86_64-w64-mingw32-gcc"
    ;;
  x86_64-unknown-linux-musl|aarch64-unknown-linux-musl)
    MUSL_CC=""
    for c in musl-gcc x86_64-linux-musl-gcc aarch64-linux-musl-gcc; do
      if command -v "$c" >/dev/null 2>&1; then MUSL_CC="$c"; break; fi
    done
    if [ -z "$MUSL_CC" ]; then
      echo "缺少 musl 交叉编译器（macOS 可 brew install messense/macos-cross-toolchains/x86_64-unknown-linux-musl）" >&2
      exit 3
    fi
    VAR="CARGO_TARGET_$(echo "$TRIPLE" | tr 'a-z-' 'A-Z_')_LINKER"
    LINKER_ENV="$VAR=$MUSL_CC"
    ;;
esac

# ---- 前端产物（embed 进主二进制）----
# 判定 assets 而非 index.html：只有 index.html 的残缺 dist 正是上次白屏的原因。
if [ ! -d "$ROOT/server/internal/webui/dist/assets" ]; then
  echo "==> 前端产物缺失，先构建前端（npm run build）"
  ( cd "$ROOT/web" && npm run build )
  rm -rf "$ROOT/server/internal/webui/dist"
  mkdir -p "$ROOT/server/internal/webui"
  cp -R "$ROOT/web/dist" "$ROOT/server/internal/webui/dist"
fi

echo "==> [1/4] 构建切片内核（$TRIPLE）"
( cd "$ROOT/kernel" && env $LINKER_ENV cargo build --release --target "$TRIPLE" )

KERNEL="$ROOT/kernel/target/$TRIPLE/release/tangis-kernel$EXE"
if [ ! -f "$KERNEL" ]; then
  echo "内核未生成：$KERNEL" >&2
  exit 4
fi

echo "==> [2/4] 构建主二进制（$GOOS_T/$GOARCH_T）"
cd "$ROOT/server"
# Windows 用 GUI 子系统：双击 tangis.exe 不弹控制台黑框（日志由 start.bat 重定向）
LDFLAGS="-s -w"
if [ "$GOOS_T" = "windows" ]; then
  LDFLAGS="-s -w -H windowsgui"
fi
rm -rf "$OUT"
mkdir -p "$OUT"
GOOS="$GOOS_T" GOARCH="$GOARCH_T" CGO_ENABLED=0 \
  go build -ldflags "$LDFLAGS" -o "$OUT/tangis$EXE" ./cmd/apiserver

echo "==> [3/4] 收集内核、启停脚本与文档"
cp "$KERNEL" "$OUT/"
if [ "$GOOS_T" = "windows" ]; then
  cp "$ROOT/deploy/desktop/start.bat" "$ROOT/deploy/desktop/stop.bat" "$OUT/"
else
  cp "$ROOT/deploy/desktop/start.sh" "$ROOT/deploy/desktop/stop.sh" "$OUT/"
  chmod +x "$OUT/start.sh" "$OUT/stop.sh"
  if [ "$GOOS_T" = "darwin" ]; then
    # .command 供 Finder 双击（无需终端）
    cp "$ROOT/deploy/desktop/start.sh" "$OUT/start.command"
    cp "$ROOT/deploy/desktop/stop.sh" "$OUT/stop.command"
    chmod +x "$OUT/start.command" "$OUT/stop.command"
  fi
fi
cp "$ROOT/deploy/desktop/README-DESKTOP.md" "$OUT/README.md"

echo "==> [4/4] 压缩分发包"
( cd "$ROOT/dist" && rm -f "tangis-$GOOS_T-$GOARCH_T.zip" && zip -qr "tangis-$GOOS_T-$GOARCH_T.zip" "tangis-$GOOS_T-$GOARCH_T" )

echo ""
echo "package : $OUT"
echo "archive : $ROOT/dist/tangis-$GOOS_T-$GOARCH_T.zip ($(du -h "$ROOT/dist/tangis-$GOOS_T-$GOARCH_T.zip" | cut -f1))"
