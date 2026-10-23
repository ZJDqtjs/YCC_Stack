#!/usr/bin/env bash
# 三个演示脚本共用的进程管理。
# 按端口精确匹配命令行来杀，不用 taskkill /IM 全机镜像名——那会顺手杀掉用户
# 正在跑的其它 Broker（比如浏览器演示时开着的那一套）。
kill_stack() {
  local port=$1
  # 必须先按进程名过滤：命令行里含 ":8787" 的还有跑这段脚本的 bash 自己，
  # 不过滤就会把调用它的 shell 顺手杀掉。
  powershell -NoProfile -Command \
    "Get-CimInstance Win32_Process -Filter \"Name='starstack-server.exe' OR Name='starstack-worker.exe'\" | Where-Object {\$_.CommandLine -match ':$port'} | ForEach-Object { Stop-Process -Id \$_.ProcessId -Force; Write-Output ('killed ' + \$_.Name + ' pid ' + \$_.ProcessId) }" 2>/dev/null || true
}

# start_stack <port> <dataDir> <logDir>：起 Broker + 两个算力节点
start_stack() {
  local port=$1 data=$2 logs=$3
  mkdir -p "$data" "$logs"
  ./bin/starstack-server.exe --data "./$data" --addr ":$port" > "$logs/server.log" 2>&1 &
  sleep 1
  ./bin/starstack-worker.exe --id node-a --server "http://127.0.0.1:$port" \
    --work "./$logs/node-a" > "$logs/node-a.log" 2>&1 &
  ./bin/starstack-worker.exe --id node-b --server "http://127.0.0.1:$port" \
    --work "./$logs/node-b" > "$logs/node-b.log" 2>&1 &
  sleep 3
}

build_stack() {
  # 同一套二进制正被另一个演示实例占用时（Windows 不允许覆盖运行中的 exe），
  # 用 SKIP_BUILD=1 复用刚编译好的那份
  [ "${SKIP_BUILD:-0}" = "1" ] && return 0
  ${GO:-go} build -o bin/starstack-server.exe ./cmd/server || exit 1
  ${GO:-go} build -o bin/starstack-worker.exe ./cmd/worker || exit 1
}
