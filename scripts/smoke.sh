#!/usr/bin/env bash
# 端到端验收：账户 -> 认领节点 -> 预估 -> 提交 -> 结算 -> 账单 -> 鉴权边界
# 自己起一套 Broker + 2 个节点（默认 8799，不和 8787 的网页演示抢），跑完打印断言。
set -uo pipefail
cd "$(dirname "$0")/.."
. scripts/stack.sh
PORT=${PORT:-8799}

build_stack
kill_stack "$PORT"
sleep 1
rm -rf smoke
mkdir -p smoke

start_stack "$PORT" smoke/data smoke

echo "== 星栈算力端到端验收 (http://127.0.0.1:$PORT) =="
python scripts/smoke.py "$PORT"
echo
echo "== broker 侧结算日志 =="
grep -E "seg .* done on|job .* done|reclaiming|stale|claimed node" smoke/server.log | tail -12
kill_stack "$PORT"
