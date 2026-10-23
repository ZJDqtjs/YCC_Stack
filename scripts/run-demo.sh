#!/usr/bin/env bash
# 星栈算力 · 转码共享 Demo 一键起：Broker + 2 个算力节点 + 登录提交任务
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/stack.sh
PORT=${PORT:-8787}
DATA=${DATA:-data}

build_stack
kill_stack "$PORT"
sleep 1
mkdir -p run
start_stack "$PORT" "$DATA" run

if [ ! -f samples/input.mp4 ]; then
  echo "[3/4] generating 60s 1080p test clip"
  mkdir -p samples
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "testsrc2=size=1920x1080:rate=30:duration=60" \
    -f lavfi -i "sine=frequency=440:duration=60" \
    -c:v libx264 -preset veryfast -crf 23 -g 60 -keyint_min 60 -sc_threshold 0 \
    -c:a aac -shortest samples/input.mp4
else
  echo "[3/4] reusing samples/input.mp4"
fi

# 任务接口要求登录：先用演示账户拿一个会话 Cookie，再提交。
# 用户名/口令保持 ASCII —— Windows 的 curl 会按本地代码页发送请求体，中文会乱码。
JAR=./run/cookies.txt
echo "[4/4] login + submitting job: ./samples/input.mp4"
curl -s -c $JAR -X POST "http://127.0.0.1:$PORT/api/auth/register" \
  -H 'Content-Type: application/json' -d '{"name":"demo","password":"demo1234"}' >/dev/null
curl -s -c $JAR -X POST "http://127.0.0.1:$PORT/api/auth/login" \
  -H 'Content-Type: application/json' -d '{"name":"demo","password":"demo1234"}' >/dev/null
# 演示账户可能在上一次运行里花光了预扣点，这里补一次值
curl -s -b $JAR -X POST "http://127.0.0.1:$PORT/api/me/recharge" \
  -H 'Content-Type: application/json' -d '{"amount":200}' >/dev/null
curl -s -b $JAR -X POST "http://127.0.0.1:$PORT/api/jobs" \
  -F "path=./samples/input.mp4" -F "vcodec=libx264" -F "preset=veryfast" -F "q=28" \
  -F "height=720" -F "segs=4" -F "audio=1" -F "baseline=1"
echo
echo "dashboard: http://127.0.0.1:$PORT/   (demo / demo1234，Ctrl-C to stop everything)"
trap 'kill_stack "$PORT"' EXIT
wait
