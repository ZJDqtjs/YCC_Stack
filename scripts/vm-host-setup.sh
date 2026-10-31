#!/bin/sh
# StarStack VM host setup / preflight (Linux and macOS).
#
# Checks QEMU, suggests or installs it with the OS package manager, then starts
# the provider agent with --vm. The web console's one-click launcher does the
# same inline; this script is for operators who already have the worker binary.
#
# Example:
#   ./vm-host-setup.sh --server http://broker:8787 --id dorm-pc \
#     --launch A1B2C3D4 --vm-max 2
#   ./vm-host-setup.sh --server http://broker:8787 --id dorm-pc \
#     --bridge br0 --uplink eth0 --yes
set -eu

SERVER=""
NODE_ID="$(hostname 2>/dev/null || echo starstack-host)"
LAUNCH=""
TOKEN=""
NAME=""
PRICE="0.8"
SLOTS="2"
VMMAX="2"
BRIDGE=""
UPLINK=""
WORKER="starstack-worker"
WORK="${HOME:-.}/.starstack-node/cache"
YES=0
CHECK_ONLY=0

usage() {
  cat <<'EOF'
Usage: vm-host-setup.sh --server URL [options]

  --server URL       Broker base URL (required)
  --id ID            Node id (default: hostname)
  --launch CODE      One-use launch code from the web console
  --token TOKEN      Shared agent token (if the Broker uses one)
  --name NAME        Display name for this machine
  --price N          Transcode price, points / video-second (default 0.8)
  --slots N          Transcode concurrency slots (default 2)
  --vm-max N         Max concurrent VMs (default 2, max 8)
  --bridge NAME      Linux routed mode bridge (e.g. br0); empty = slirp
  --uplink NAME      Linux uplink interface for NDP proxy (e.g. eth0)
  --worker PATH      Agent binary (default: starstack-worker in PATH)
  --work DIR         Agent cache directory
  --yes              Install QEMU without asking (if a package manager is known)
  --check-only       Only run the preflight, do not start the agent
  -h, --help         Show this help
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER=$2; shift 2 ;;
    --id) NODE_ID=$2; shift 2 ;;
    --launch) LAUNCH=$2; shift 2 ;;
    --token) TOKEN=$2; shift 2 ;;
    --name) NAME=$2; shift 2 ;;
    --price) PRICE=$2; shift 2 ;;
    --slots) SLOTS=$2; shift 2 ;;
    --vm-max) VMMAX=$2; shift 2 ;;
    --bridge) BRIDGE=$2; shift 2 ;;
    --uplink) UPLINK=$2; shift 2 ;;
    --worker) WORKER=$2; shift 2 ;;
    --work) WORK=$2; shift 2 ;;
    --yes) YES=1; shift ;;
    --check-only) CHECK_ONLY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

[ -n "$SERVER" ] || { echo "--server is required" >&2; usage; exit 2; }
case "$VMMAX" in ''|*[!0-9]*) echo "--vm-max must be a number" >&2; exit 2 ;; esac
[ "$VMMAX" -ge 1 ] && [ "$VMMAX" -le 8 ] || { echo "--vm-max must be 1..8" >&2; exit 2; }

OS=$(uname -s 2>/dev/null || echo unknown)
SUDO=""
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
  SUDO="sudo"
fi

install_cmd() {
  if command -v apt-get >/dev/null 2>&1; then
    echo "$SUDO apt-get update && $SUDO apt-get install -y qemu-system-x86 qemu-utils"
  elif command -v dnf >/dev/null 2>&1; then
    echo "$SUDO dnf install -y @virtualization"
  elif command -v pacman >/dev/null 2>&1; then
    echo "$SUDO pacman -S --noconfirm qemu-full"
  elif command -v brew >/dev/null 2>&1; then
    echo "brew install qemu"
  fi
}

QEMU_BIN=qemu-system-x86_64
if ! command -v "$QEMU_BIN" >/dev/null 2>&1 || ! command -v qemu-img >/dev/null 2>&1; then
  CMD=$(install_cmd)
  if [ -z "$CMD" ]; then
    echo "[!] 找不到 QEMU，也没有识别到可用的包管理器。"
    echo "    请从 https://www.qemu.org/download/ 安装 qemu-system-x86_64 与 qemu-img 后重试。"
    exit 1
  fi
  echo "[!] 云主机需要 QEMU，但当前环境缺少 $QEMU_BIN 或 qemu-img。"
  echo "    安装命令：$CMD"
  if [ "$YES" -eq 1 ]; then
    sh -c "$CMD"
  elif [ -t 0 ]; then
    printf "现在安装 QEMU？[y/N] "
    read -r ans
    case "$ans" in
      y|Y) sh -c "$CMD" ;;
      *) echo "已取消。装好 QEMU 后重新运行本脚本。"; exit 1 ;;
    esac
  else
    echo "非交互环境，请手动执行上面的命令后重试。"
    exit 1
  fi
fi
command -v "$QEMU_BIN" >/dev/null 2>&1 || { echo "[!] QEMU 仍未安装，退出。"; exit 1; }
command -v qemu-img >/dev/null 2>&1 || { echo "[!] 找不到 qemu-img，请安装 qemu-utils/qemu-img。"; exit 1; }

case "$OS" in
  Linux)
    if [ -e /dev/kvm ] && [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
      echo "KVM 可用，虚拟机使用硬件加速。"
    else
      echo "[!] /dev/kvm 不可用：虚拟机将以 TCG 软件模拟运行，速度会明显变慢。"
      echo "    sudo usermod -aG kvm \$USER 后重新登录，或在 BIOS/UEFI 里开启虚拟化。"
    fi
    ;;
  Darwin)
    echo "macOS 会使用 HVF；x86_64 客户机在部分机型上会退回 TCG，速度取决于机器。"
    ;;
  *)
    echo "[!] 云主机模式目前只支持 Linux/macOS；当前系统：$OS" >&2
    exit 1
    ;;
esac

if [ -n "$BRIDGE" ]; then
  if [ "$OS" != "Linux" ]; then
    echo "[!] routed 网桥只支持 Linux；当前系统请留空网桥使用 slirp。" >&2
    exit 1
  fi
  [ -d "/sys/class/net/$BRIDGE" ] || { echo "[!] 找不到网桥 $BRIDGE" >&2; exit 1; }
  if [ -n "$UPLINK" ] && [ ! -d "/sys/class/net/$UPLINK" ]; then
    echo "[!] 找不到上联网卡 $UPLINK" >&2
    exit 1
  fi
  if [ "$(id -u)" -ne 0 ]; then
    echo "[!] routed 模式需要 root 或 CAP_NET_ADMIN；当前不是 root，请用 sudo 运行或留空网桥改用 slirp。"
  fi
fi

echo "防火墙/安全组请放行 IPv6 入站 TCP 5900-5999（控制台）与 20000-29999（SSH/端口映射）。"

WORKER_BIN=$WORKER
if [ ! -f "$WORKER_BIN" ] && [ -x "$HOME/.starstack-node/starstack-worker" ]; then
  WORKER_BIN="$HOME/.starstack-node/starstack-worker"
fi
if [ -f "$WORKER_BIN" ]; then
  :
elif command -v "$WORKER_BIN" >/dev/null 2>&1; then
  WORKER_BIN=$(command -v "$WORKER_BIN")
else
  echo "[!] 找不到 Agent 程序：$WORKER" >&2
  echo "    从控制台的云主机页下载启动脚本，或用 --worker 指定 starstack-worker 的路径。" >&2
  exit 1
fi

if [ "$CHECK_ONLY" -eq 1 ]; then
  echo "预检通过；未启动 Agent（--check-only）。"
  exit 0
fi

mkdir -p "$WORK"
set -- --server "$SERVER" --id "$NODE_ID" --price "$PRICE" --slots "$SLOTS" \
  --work "$WORK" --vm --vm-max "$VMMAX" --qemu "$QEMU_BIN" --qemu-img qemu-img
if [ -n "$NAME" ]; then set -- "$@" --name "$NAME"; fi
if [ -n "$LAUNCH" ]; then set -- "$@" --launch "$LAUNCH"; fi
if [ -n "$TOKEN" ]; then set -- "$@" --token "$TOKEN"; fi
if [ -n "$BRIDGE" ]; then set -- "$@" --vm-bridge "$BRIDGE"; fi
if [ -n "$UPLINK" ]; then set -- "$@" --vm-uplink "$UPLINK"; fi

echo "启动云主机 Agent：$WORKER_BIN（最多 $VMMAX 台）"
exec "$WORKER_BIN" "$@"
