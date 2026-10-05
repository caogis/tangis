#!/usr/bin/env bash
# 停止本机运行的 TanGIS 桌面单机版（macOS / Linux）
cd "$(dirname "$0")"
# pid 可能在安装目录（旧版）或数据目录（当前版，避免打包覆盖丢失）
for f in "${TANGIS_HOME:-$HOME/TanGIS}/tangis.pid" ./tangis.pid; do
  [ -f "$f" ] || continue
  PID="$(cat "$f")"
  if kill -0 "${PID}" 2>/dev/null; then
    kill "${PID}" && echo "已停止 TanGIS (pid ${PID})"
    rm -f "$f"
    exit 0
  fi
  rm -f "$f"
done
# 兜底：按进程名清理（仅在上述 pid 文件均缺失时使用）。
# 注意匹配「以 tangis 结尾」而非固定目录名：从安装目录相对启动时
# 命令行是 "./tangis"，用目录名匹配会漏杀，导致端口被旧进程占住。
if pkill -f "(^|/)tangis(\.exe)?$" 2>/dev/null; then
  echo "已停止 TanGIS"
else
  echo "未发现运行中的 TanGIS"
fi
