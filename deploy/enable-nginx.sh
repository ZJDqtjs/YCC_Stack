#!/usr/bin/env bash
# 把 Broker 挂到这台机器上已有的 :80 nginx 站点下（路径前缀 /starstack/）。
#
# 这一步改的是别人在用的服务：erp 站点的 server 块要新增一行 include。脚本会先备份、
# 装好后跑 nginx -t，配置不通就立刻回滚，不会 reload。因此它默认拒绝执行，必须显式
# 带 --yes。
#
# 另一条路是不动 nginx、在 Azure NSG 上放行一个端口（8787 实测被 NSG 挡在外面），
# 那要在 Azure 门户里操作，SSH 进去改不了。
set -uo pipefail
cd "$(dirname "$0")"

# 目标机器不写死在脚本里：这是公开仓库，默认值会变成一台正在跑的服务器的地址和
# 登录用户。三个变量都得自己给（KEY 是 SSH 私钥路径）。
KEY="${KEY:?用法: KEY=~/.ssh/id_ed25519 HOST=1.2.3.4 USER_NAME=deploy bash deploy/enable-nginx.sh --yes}"
HOST="${HOST:?请设置 HOST（服务器地址）}"
USER_NAME="${USER_NAME:?请设置 USER_NAME（SSH 登录用户）}"
SSH="ssh -i $KEY -o StrictHostKeyChecking=accept-new $USER_NAME@$HOST"
SITE=/etc/nginx/sites-enabled/erp

[ "${1:-}" = "--yes" ] || {
  echo "这会修改生产站点 $SITE（加一行 include，然后 nginx -t + reload）。"
  echo "确认无误后执行： bash deploy/enable-nginx.sh --yes"
  exit 1
}

scp -i "$KEY" -o StrictHostKeyChecking=accept-new nginx-starstack.conf "$USER_NAME@$HOST:/tmp/" || exit 1

# 远端整段跑在 sudo bash 里；heredoc 用引号定界，本地不做任何展开，
# 免得 sed 的 | 分隔符和 $ 在两层 shell 之间被吃掉一次。
$SSH 'sudo bash -s' <<REMOTE || exit 1
set -uo pipefail
SITE=/etc/nginx/sites-enabled/erp
mv -f /tmp/nginx-starstack.conf /etc/nginx/snippets/starstack.conf
chmod 0644 /etc/nginx/snippets/starstack.conf

BAK="\$SITE.bak-\$(date +%s)"
cp -a "\$SITE" "\$BAK"

if grep -q 'snippets/starstack.conf' "\$SITE"; then
  echo "已经 include 过，配置不动"
else
  # 与 erp-backup-receiver 那行同级，插在它前面；include 不是 location，
  # 位置不影响匹配优先级
  sed -i '\|^    include /etc/nginx/snippets/erp-backup-receiver.conf;|i\    include /etc/nginx/snippets/starstack.conf;' "\$SITE"
fi

if nginx -t; then
  systemctl reload nginx
  echo "nginx 已 reload（备份在 \$BAK）"
else
  echo "nginx -t 未通过，回滚"
  cp -a "\$BAK" "\$SITE"
  nginx -t >/dev/null 2>&1 && systemctl reload nginx
  exit 1
fi
REMOTE

echo "== 公网自检 =="
curl -s -m 20 -o /dev/null -w "http://$HOST/starstack/ -> %{http_code}\n" "http://$HOST/starstack/"
curl -s -m 20 -o /dev/null -w "erp 首页未受影响 -> %{http_code}\n" "http://$HOST/"
