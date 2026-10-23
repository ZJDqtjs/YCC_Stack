#!/usr/bin/env bash
# vLLM 单机多卡：张量并行把一层拆到多张卡上，适合稠密模型和显存放得下的 MoE。
set -euo pipefail

MODEL="${MODEL:-Qwen/Qwen3-30B-A3B}"
HOST="${HOST:-0.0.0.0}"
PORT="${PORT:-8000}"
TP="${TP:-2}"
MAX_LEN="${MAX_LEN:-8192}"
GPU_MEM_UTIL="${GPU_MEM_UTIL:-0.90}"

if ! command -v vllm >/dev/null 2>&1; then
  echo "未找到 vllm：pip install vllm"
  exit 1
fi

echo "启动 vLLM：TP=$TP http://$HOST:$PORT/v1"
exec vllm serve "$MODEL" --host "$HOST" --port "$PORT" \
  --tensor-parallel-size "$TP" --max-model-len "$MAX_LEN" \
  --gpu-memory-utilization "$GPU_MEM_UTIL"
