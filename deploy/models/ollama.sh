#!/usr/bin/env bash
# Ollama 也暴露 OpenAI 兼容接口（/v1），接入星栈不需要改任何代码。
set -euo pipefail

HOST="${HOST:-0.0.0.0}"
PORT="${PORT:-11434}"
MODEL="${MODEL:-}"
OLLAMA_BIN="${OLLAMA_BIN:-ollama}"

if [ -n "$MODEL" ]; then
  echo "预拉取模型 $MODEL"
  "$OLLAMA_BIN" pull "$MODEL"
fi
echo "Ollama 监听 $HOST:$PORT，OpenAI 基址 http://$HOST:$PORT/v1"
OLLAMA_HOST="$HOST:$PORT" exec "$OLLAMA_BIN" serve
