#!/usr/bin/env bash
# 在一台带 GPU 的机器上启动 llama.cpp RPC 后端。
# 它只暴露一块可被主节点使用的显存/计算池，本身不提供 OpenAI 接口。
set -euo pipefail

LLAMA_BIN_DIR="${LLAMA_BIN_DIR:-$(pwd)}"
RPC_HOST="${RPC_HOST:-0.0.0.0}"
RPC_PORT="${RPC_PORT:-50052}"
RPC_BIN="${RPC_BIN:-$LLAMA_BIN_DIR/rpc-server}"

if [ ! -x "$RPC_BIN" ] && command -v rpc-server >/dev/null 2>&1; then
  RPC_BIN="$(command -v rpc-server)"
fi
if [ ! -x "$RPC_BIN" ]; then
  echo "找不到 rpc-server：请先编译 llama.cpp（cmake --build build --target rpc-server），"
  echo "或用 LLAMA_BIN_DIR 指向 build/bin。"
  exit 1
fi

echo "RPC 后端监听 $RPC_HOST:$RPC_PORT（确保主节点能访问这个端口）"
exec "$RPC_BIN" -H "$RPC_HOST" -p "$RPC_PORT"
