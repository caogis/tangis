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

# ---- 版本号：默认读源码里的 api.Version，可用 VERSION= 覆盖 ----
# 注入二进制后 GET /api/v1/system 会回显它；Info.plist 用它的数字部分
# （macOS 要求 CFBundleShortVersionString 形如 x.y.z，不接受 "0.1.0-dev"）。
VER="${VERSION:-}"
if [ -z "$VER" ]; then
  VER="$(sed -n 's/^var Version = "\(.*\)"/\1/p' "$ROOT/server/internal/api/control.go" | head -1)"
fi
[ -z "$VER" ] && VER="0.0.0-dev"
PLIST_VER="$(echo "$VER" | sed -n 's/^\([0-9]*\.[0-9]*\.[0-9]*\).*/\1/p')"
[ -z "$PLIST_VER" ] && PLIST_VER="0.0.0"

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

echo "==> 版本：$VER（plist $PLIST_VER）"

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
LDFLAGS="-s -w -X tangis/server/internal/api.Version=$VER"
if [ "$GOOS_T" = "windows" ]; then
  LDFLAGS="$LDFLAGS -H windowsgui"
fi
rm -rf "$OUT"
mkdir -p "$OUT"
GOOS="$GOOS_T" GOARCH="$GOARCH_T" CGO_ENABLED=0 \
  go build -ldflags "$LDFLAGS" -o "$OUT/tangis$EXE" ./cmd/apiserver

# ---- macOS：打成可双击的 .app ----
#
# 为什么要有 .app：目录形态要求用户"先解压、再双击 start.command"，会弹出一个终端
# 窗口，Finder 里也没有图标、拖不进"应用程序"。.app 把二进制收进 Contents/Resources，
# 双击图标即用，且整个 .app 可以拖进 /Applications。
#
# 二进制只保留一份（在 .app 内），顶层的 start.sh/start.command 转调 .app 内的脚本，
# 因此命令行用法与过去完全一致，且不会让压缩包体积翻倍。
# 数据目录与 .app 位置无关（仍是 ~/TanGIS），换目录、换机器都不丢数据。
make_macos_app() {
  app="$OUT/TanGIS.app"
  rm -rf "$app"
  mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

  # 主二进制已在 $OUT；内核直接取构建产物（darwin 走 .app，不经 $OUT 中转）
  mv "$OUT/tangis" "$app/Contents/Resources/"
  cp "$KERNEL" "$app/Contents/Resources/"
  cp "$ROOT/deploy/desktop/start.sh" "$app/Contents/Resources/start.sh"
  chmod +x "$app/Contents/Resources/start.sh" "$app/Contents/Resources/tangis" \
    "$app/Contents/Resources/tangis-kernel"

  cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>TanGIS</string>
  <key>CFBundleDisplayName</key><string>TanGIS</string>
  <key>CFBundleIdentifier</key><string>com.tangis.desktop</string>
  <key>CFBundleExecutable</key><string>TanGIS</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$PLIST_VER</string>
  <key>CFBundleVersion</key><string>$PLIST_VER</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>LSApplicationCategoryType</key><string>public.app-category.developer-tools</string>
</dict>
</plist>
PLIST
  plutil -lint "$app/Contents/Info.plist" >/dev/null || {
    echo "Info.plist 校验失败" >&2
    exit 5
  }

  # 图标：缺 Pillow/iconutil 时跳过——图标是锦上添花，不该挡住产物
  if python3 -c 'import PIL' >/dev/null 2>&1; then
    python3 "$ROOT/deploy/desktop/make-icon.py" "$app/Contents/Resources/AppIcon.icns" || true
  else
    echo "    (跳过图标：未安装 Pillow)"
  fi

  # 启动器。转调已测过的 start.sh；失败必须弹原生对话框——.app 没有终端，
  # 静默失败会让用户以为"双击没反应"。启动输出落到数据目录便于排查。
  cat > "$app/Contents/MacOS/TanGIS" <<'LAUNCH'
#!/bin/bash
set -uo pipefail
RES="$(cd "$(dirname "$0")/../Resources" && pwd)"
LOG_DIR="${TANGIS_HOME:-$HOME/TanGIS}"
mkdir -p "$LOG_DIR"
if ! "$RES/start.sh" >>"$LOG_DIR/start.log" 2>&1; then
  osascript -e 'display dialog "TanGIS 启动失败，请查看日志 ~/TanGIS/start.log" buttons {"好"} default button 1 with icon stop' >/dev/null 2>&1 || true
  exit 1
fi
LAUNCH
  chmod +x "$app/Contents/MacOS/TanGIS"

  # 顶层入口：转调 .app 内脚本，保持 `./start.sh` / `./start.sh --force` 等价可用
  cat > "$OUT/start.sh" <<'WRAP'
#!/usr/bin/env bash
# 目录形态的启动入口：转调 TanGIS.app 内的启动脚本（二进制只保留在 .app 内）。
# 用法与直接运行 ./TanGIS.app/Contents/Resources/start.sh 完全一致。
set -euo pipefail
cd "$(dirname "$0")"
exec ./TanGIS.app/Contents/Resources/start.sh "$@"
WRAP
  cp "$OUT/start.sh" "$OUT/start.command"
  cp "$ROOT/deploy/desktop/stop.sh" "$OUT/stop.sh"
  cp "$ROOT/deploy/desktop/stop.sh" "$OUT/stop.command"
  chmod +x "$OUT/start.sh" "$OUT/start.command" "$OUT/stop.sh" "$OUT/stop.command"

  # 临时签名（ad-hoc）：Apple Silicon 上被破坏签名的可执行文件会被直接拒绝运行。
  # 失败不阻断——x86_64 或未装签名工具时产物仍可用。
  if command -v codesign >/dev/null 2>&1; then
    codesign --force --sign - "$app/Contents/Resources/tangis" >/dev/null 2>&1 || true
    codesign --force --sign - "$app/Contents/Resources/tangis-kernel" >/dev/null 2>&1 || true
    if codesign --force --sign - "$app" >/dev/null 2>&1; then
      echo "    .app 已做临时签名（未做 Apple 公证，首次打开可能需右键→打开）"
    else
      echo "    (bundle 签名失败；x86_64 上未签名仍可运行)"
    fi
  fi
  echo "    app     : $app"
}

echo "==> [3/4] 收集内核、启停脚本与文档"
if [ "$GOOS_T" = "darwin" ]; then
  make_macos_app
else
  cp "$KERNEL" "$OUT/"
  if [ "$GOOS_T" = "windows" ]; then
    cp "$ROOT/deploy/desktop/start.bat" "$ROOT/deploy/desktop/stop.bat" "$OUT/"
  else
    cp "$ROOT/deploy/desktop/start.sh" "$ROOT/deploy/desktop/stop.sh" "$OUT/"
    chmod +x "$OUT/start.sh" "$OUT/stop.sh"
  fi
fi
cp "$ROOT/deploy/desktop/README-DESKTOP.md" "$OUT/README.md"

echo "==> [4/4] 压缩分发包"
( cd "$ROOT/dist" && rm -f "tangis-$GOOS_T-$GOARCH_T.zip" && zip -qry "tangis-$GOOS_T-$GOARCH_T.zip" "tangis-$GOOS_T-$GOARCH_T" )
# 校验和：分发件常有"下载不完整/被改动"的纠纷，附一份 sha256 省得排查
( cd "$ROOT/dist" && shasum -a 256 "tangis-$GOOS_T-$GOARCH_T.zip" > "tangis-$GOOS_T-$GOARCH_T.zip.sha256" )

echo ""
echo "package : $OUT"
echo "archive : $ROOT/dist/tangis-$GOOS_T-$GOARCH_T.zip ($(du -h "$ROOT/dist/tangis-$GOOS_T-$GOARCH_T.zip" | cut -f1))"
echo "sha256  : $(cut -d' ' -f1 < "$ROOT/dist/tangis-$GOOS_T-$GOARCH_T.zip.sha256")"
