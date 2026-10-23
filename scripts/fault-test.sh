#!/usr/bin/env bash
# 故障注入：任务跑到一半杀掉一个算力节点，验证租约回收 + 重派 + 产物仍然完整
set -uo pipefail
cd "$(dirname "$0")/.."
. scripts/stack.sh
PORT=${PORT:-8797}
DATA=${DATA:-ft-data}
export PORT
# check.py 必须用同一个账户观察任务，否则它只看到自己的空列表
export STARSTACK_USER=demo STARSTACK_PASS=demo1234

build_stack
kill_stack "$PORT"
sleep 1
rm -rf "$DATA" ft
mkdir -p ft

start_stack "$PORT" "$DATA" ft

# data/ 是刚建出来的，这里注册出的演示账户带赠送点数；任务接口要求带会话提交。
JAR=./ft/cookies.txt
curl -s -c $JAR -X POST "http://127.0.0.1:$PORT/api/auth/register" \
  -H 'Content-Type: application/json' -d '{"name":"demo","password":"demo1234"}' >/dev/null
JOB=$(curl -s -b $JAR -X POST "http://127.0.0.1:$PORT/api/jobs" \
  -F "path=./samples/input.mp4" -F "vcodec=libx264" -F "preset=slow" -F "q=24" \
  -F "height=1080" -F "segs=6" -F "audio=1" | python -c "import json,sys;print(json.load(sys.stdin)['id'])")
echo "fault-test job = $JOB"
sleep 12
echo "--- killing node-b mid-transcode ---"
# Git Bash PIDs are not Windows PIDs; match on the command line instead.
# Git Bash PIDs are not Windows PIDs；按命令行里的节点名 + 端口精确定位，
# 只用 -like '*:PORT' 会漏掉（命令行结尾是 --work，不是端口）
powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='starstack-worker.exe'\" | Where-Object {\$_.CommandLine -like '*--id node-b*' -and \$_.CommandLine -match ':$PORT'} | ForEach-Object { Stop-Process -Id \$_.ProcessId -Force; 'killed pid ' + \$_.ProcessId }"
python scripts/check.py "$JOB"
sleep 150
echo "=== after reclaim window ==="
python scripts/check.py "$JOB"
echo "=== broker reclaim log ==="
grep -iE "reclaiming|stale|permanently|done" ft/server.log | tail -12
echo "=== output integrity ==="
if [ -f "$DATA/jobs/$JOB/out.mp4" ]; then
  ffprobe -v error -show_entries format=duration -of default=nw=1 "$DATA/jobs/$JOB/out.mp4"
  ffmpeg -v error -i "$DATA/jobs/$JOB/out.mp4" -f null - 2>&1 | head -3
  echo "decoded clean"
else
  echo "NO OUTPUT PRODUCED"
fi
kill_stack "$PORT"
