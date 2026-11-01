#!/bin/sh
REPO_URL="https://raw.githubusercontent.com/dontbass/hlsproxy/main"
wget -qO /usr/bin/hlsproxy $REPO_URL/bin/hlsproxy_aarch64
chmod +x /usr/bin/hlsproxy
/etc/init.d/hlsproxy restart
echo "HLS Proxy успешно обновлен и перезапущен!"
