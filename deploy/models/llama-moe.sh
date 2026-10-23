#!/usr/bin/env bash
# 单机 MoE：GPU 放注意力层，部分专家层放 CPU 内存。
# 对星栈来说它就是一个普通的 OpenAI 兼容上游（http://host:8080）。
set -euo pipefail

LLAMA_BIN="${LLAMA_BIN:-llama-server}"
MODEL="${MODEL:-./Qwen3-30B-A3B-Q4_K_M.gguf}"
HOST="${HOST:-0.0.0.0}"
PORT="${PORT:-8080}"
CTX="${CTX:-8192}"
NGL="${NGL:-99}"
N_CPU_MOE="${N_CPU_MOE:-24}"     # 放到 CPU 的 MoE 层数；显存越小调大
THREADS="${THREADS:-$(nproc 2>/dev/null || echo 8)}"
API_KEY="${API_KEY:-}"           # 可选：llama.cpp --api-key

if [ ! -f "$MODEL" ]; then
  echo "模型文件不存在：$MODEL"
  echo "例：huggingface-cli download Qwen/Qwen3-30B-A3B-GGUF Qwen3-30B-A3B-Q4_K_M.gguf --local-dir ."
  exit 1
fi

args=(-m "$MODEL" --host "$HOST" --port "$PORT" -c "$CTX" -ngl "$NGL"
      --n-cpu-moe "$N_CPU_MOE" -t "$THREADS" --jinja)
[ -n "$API_KEY" ] && args+=(--api-key "$API_KEY")

echo "启动 llama-server：${args[*]}"
echo "OpenAI 端点：http://$HOST:$PORT/v1"
exec "$LLAMA_BIN" "${args[@]}"
