#!/bin/sh
REPO_URL="https://raw.githubusercontent.com/dontbass/hlsproxy/main"
wget -qO /usr/bin/hlsproxy $REPO_URL/bin/hlsproxy_aarch64
chmod +x /usr/bin/hlsproxy
wget -qO /etc/init.d/hlsproxy $REPO_URL/init.d/hlsproxy
chmod +x /etc/init.d/hlsproxy
/etc/init.d/hlsproxy enable
/etc/init.d/hlsproxy start
mkdir -p /usr/lib/lua/luci/controller /usr/lib/lua/luci/view/hlsproxy
wget -qO /usr/lib/lua/luci/controller/hlsproxy.lua $REPO_URL/luci/controller/hlsproxy.lua
wget -qO /usr/lib/lua/luci/view/hlsproxy/index.htm $REPO_URL/luci/view/hlsproxy/index.htm
rm -f /tmp/luci-indexcache*
echo "Установка завершена! Зайди в веб-интерфейс роутера: Службы -> HLS Proxy"
