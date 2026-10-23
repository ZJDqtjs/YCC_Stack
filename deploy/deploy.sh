#!/usr/bin/env bash
# 把 Broker 推到服务器上：只做加法——新增 /opt/starstack、/var/lib/starstack、
# 一个 systemd 单元，不碰这台机器上已经在跑的 erp 服务和它的 nginx 配置。
# 公网入口是另一个决定，见 enable-nginx.sh。
set -uo pipefail
cd "$(dirname "$0")"

# 目标机器不写死在脚本里：这是公开仓库，默认值会变成一台正在跑的服务器的地址和
# 登录用户。三个变量都得自己给（KEY 是 SSH 私钥路径，BASE_URL 见下面第 3 步）。
KEY="${KEY:?用法: KEY=~/.ssh/id_ed25519 HOST=1.2.3.4 USER_NAME=deploy bash deploy/deploy.sh}"
HOST="${HOST:?请设置 HOST（服务器地址）}"
USER_NAME="${USER_NAME:?请设置 USER_NAME（SSH 登录用户）}"
SSH="ssh -i $KEY -o StrictHostKeyChecking=accept-new $USER_NAME@$HOST"

# sudo 只作用于它后面那一条命令，所以多行脚本必须整段塞进 `sudo bash -s` 的 stdin：
# 写成 ssh host sudo 'cmd1; cmd2' 时 cmd2 是以普通用户身份跑的，
# 表现就是 useradd/install 报 Permission denied 而脚本继续往下走。
sudo_run() { $SSH 'sudo bash -s' <<< "$1"; }

echo "== 1/5 交叉编译 =="
cd ..
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/linux-amd64/starstack-server ./cmd/server || exit 1
# 节点程序是网页「一键提供算力」要下发的东西，六个平台都得有；
# -s -w 剥掉符号表和 DWARF，20MB 出头的二进制在慢出口上少下载几分钟。
for t in linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64; do
  os=${t%/*}; arch=${t#*/}
  ext=""; [ "$os" = windows ] && ext=.exe
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags="-s -w" \
    -o dist/dl/starstack-worker-$os-$arch$ext ./cmd/worker || exit 1
done
cd deploy

echo "== 2/5 依赖检查 =="
$SSH 'free -m | sed -n 2p; df -h / | tail -1'
if ! $SSH 'command -v ffprobe' >/dev/null; then
  echo "缺少 ffmpeg/ffprobe：Broker 要用 ffprobe 读时长与关键帧、用 ffmpeg 拼接产物，"
  echo "没装的话每一个任务都会在登记阶段被拒。安装它会动这台机器的软件包，所以默认不替你做："
  echo "    sudo apt-get install -y ffmpeg"
  echo "确认要脚本代装： FORCE_FFMPEG=1 bash deploy/deploy.sh"
  [ "${FORCE_FFMPEG:-0}" = 1 ] || exit 1
  sudo_run 'DEBIAN_FRONTEND=noninteractive apt-get install -y ffmpeg' || exit 1
fi
$SSH 'echo "ffmpeg: $(ffmpeg -version | head -1)"'

echo "== 3/5 上传二进制 =="
# 六个平台一起传：scp 对每个文件开一条通道，串行跑会把上传时间加六倍，
# 所以先打成一个 tar 再传一次。运维自己要用节点程序时取
# /opt/starstack/dl/starstack-worker-linux-amd64 即可。
# 单元文件里的 --base-url 是占位符：它是 Broker 向 worker 公布的对外地址，
# 默认用本次部署的 HOST，想走域名或 nginx 前缀就显式给 BASE_URL。
sed "s|__STARSTACK_BASE_URL__|${BASE_URL:-http://$HOST:8787}|" \
  starstack.service > /tmp/starstack.service || exit 1
tar -C ../dist/dl -cf /tmp/starstack-nodedl.tar . || exit 1
scp -i "$KEY" -o StrictHostKeyChecking=accept-new \
  ../dist/linux-amd64/starstack-server /tmp/starstack-nodedl.tar \
  /tmp/starstack.service "$USER_NAME@$HOST:/tmp/" || exit 1
sudo_run 'getent passwd starstack >/dev/null || useradd -r -s /usr/sbin/nologin -d /var/lib/starstack starstack
  install -d -m 0755 -o starstack -g starstack /opt/starstack
  install -d -m 0755 -o starstack -g starstack /opt/starstack/dl
  install -d -m 0700 -o starstack -g starstack /var/lib/starstack
  # rename() 可以覆盖一个正在运行的可执行文件（旧 inode 留给还在跑的进程），
  # 直接 cp 进去则会 ETXTBSY，所以先落到 /tmp 再 mv。
  mv -f /tmp/starstack-server /opt/starstack/starstack-server
  chmod 0755 /opt/starstack/starstack-server
  tar -xf /tmp/starstack-nodedl.tar -C /opt/starstack/dl
  rm -f /tmp/starstack-nodedl.tar
  chmod 0755 /opt/starstack/dl/*' || exit 1

echo "== 4/5 配置与令牌 =="
sudo_run 'install -d -m 0750 -o root -g starstack /etc/starstack
  if [ ! -f /etc/starstack/env ]; then
    printf "STARSTACK_AGENT_TOKEN=%s\n" "$(openssl rand -hex 16)" > /etc/starstack/env
    chmod 0640 /etc/starstack/env
    chown root:starstack /etc/starstack/env
    echo "已生成 /etc/starstack/env"
  else
    echo "/etc/starstack/env 已存在，保持不变"
  fi
  install -m 0644 -o root -g root /tmp/starstack.service /etc/systemd/system/starstack.service
  rm -f /tmp/starstack.service
  systemctl daemon-reload' || exit 1

echo "== 5/5 启动 =="
sudo_run 'systemctl enable starstack >/dev/null 2>&1
  systemctl restart starstack
  sleep 2; systemctl is-active starstack; journalctl -u starstack -n 8 --no-pager -o cat'
$SSH 'printf "本机 8787 -> "; curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8787/api/state'
curl -s -m 20 -o /dev/null -w "公网 8787  -> %{http_code}（000 = NSG 还没放开该端口）\n" \
  "http://$HOST:8787/api/state" || true
echo
echo "接入令牌（Provider Agent 的 --token，只有手动方式才需要）："
sudo_run 'sed -n "s/^STARSTACK_AGENT_TOKEN=//p" /etc/starstack/env'
echo
echo "网页里的「一键提供算力」不需要这个令牌：登录后点下载启动脚本即可。"
echo "节点程序目录（/dl/ 提供的就是这些）："
$SSH 'ls -lh /opt/starstack/dl | tail -n +2'
echo
echo "日志： $SSH 'sudo journalctl -u starstack -f'"
