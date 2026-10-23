#!/usr/bin/env bash
# 多机分布式：主节点用 --rpc 聚合若干台 rpc-server 的显存，
# 再把放不下的专家层压到 CPU。买家仍然只连主节点的 /v1。
set -euo pipefail

LLAMA_BIN="${LLAMA_BIN:-llama-server}"
MODEL="${MODEL:-./Qwen3-30B-A3B-Q4_K_M.gguf}"
RPC="${RPC:?请设置 RPC，例如 192.168.1.10:50052,192.168.1.11:50052}"
HOST="${HOST:-0.0.0.0}"
PORT="${PORT:-8080}"
CTX="${CTX:-8192}"
NGL="${NGL:-99}"
N_CPU_MOE="${N_CPU_MOE:-16}"
THREADS="${THREADS:-$(nproc 2>/dev/null || echo 8)}"
API_KEY="${API_KEY:-}"

if [ ! -f "$MODEL" ]; then
  echo "模型文件不存在：$MODEL"
  exit 1
fi

args=(-m "$MODEL" --host "$HOST" --port "$PORT" -c "$CTX" -ngl "$NGL"
      --rpc "$RPC" --n-cpu-moe "$N_CPU_MOE" -t "$THREADS" --jinja)
[ -n "$API_KEY" ] && args+=(--api-key "$API_KEY")

echo "聚合 RPC：$RPC"
echo "启动 llama-server：${args[*]}"
exec "$LLAMA_BIN" "${args[@]}"
