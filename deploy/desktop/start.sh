#!/usr/bin/env bash
# TanGIS 桌面单机版启动脚本（macOS / Linux）
#
# 用法：双击运行（macOS 为 start.command）或在终端执行 ./start.sh
# 默认监听固定端口 8080，启动后自动打开浏览器；关闭终端不会停止服务。
#
# 可覆盖环境变量：
#   PORT=9000 ./start.sh        # 换端口
#   TANGIS_HOME=/data/tangis    # 换数据目录（默认 ~/TanGIS）
set -euo pipefail

cd "$(dirname "$0")"
PORT="${PORT:-8080}"
export TANGIS_MODE=desktop
export PORT

# --force：先停掉在运行的实例再启动（升级二进制后需要，否则会因端口被占而沿用旧进程）
if [ "${1:-}" = "--force" ]; then
  "$(dirname "$0")/stop.sh" >/dev/null 2>&1 || true
  sleep 1
fi

BIN="./tangis"
DATA_DIR="${TANGIS_HOME:-$HOME/TanGIS}"
# pid 与日志都放**数据目录**，不放安装目录：
#   1) 安装目录可能被重新打包覆盖，pid 丢失会导致 stop 失效、旧二进制继续占端口；
#   2) macOS 的 .app 形态下"安装目录"就是 bundle 内部（Contents/Resources）——
#      往里写日志会破坏临时签名，而且用户把 .app 拖进 /Applications 后，
#      bundle 本应被当作只读。
LOG="${DATA_DIR}/tangis.log"
PID_FILE="${DATA_DIR}/tangis.pid"

echo "TanGIS 正在启动（端口 ${PORT}）..."

# 已运行则直接打开浏览器，避免重复起进程占用端口
if curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
  echo "检测到 TanGIS 已在运行，直接打开控制台。"
else
  mkdir -p "${DATA_DIR}"
  nohup "${BIN}" >"${LOG}" 2>&1 &
  echo $! > "${PID_FILE}"
  for _ in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then break; fi
    sleep 1
  done
fi

if ! curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
  echo "启动失败，请查看日志：${LOG}"
  exit 1
fi

URL="http://127.0.0.1:${PORT}/"
echo "---------------------------------------------"
echo " TanGIS 已就绪"
echo " 控制台：${URL}"
echo " 数据目录：${DATA_DIR}"
echo " 日志：${LOG}"
echo " 停止：./stop.sh（pid 记录：${PID_FILE}）"
echo "---------------------------------------------"

case "$(uname -s)" in
  Darwin) open "${URL}" ;;
  *)      command -v xdg-open >/dev/null 2>&1 && xdg-open "${URL}" || true ;;
esac
