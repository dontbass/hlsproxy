#!/bin/sh
# HLS Proxy (анти-фриз) — установка/обновление:
#   wget -qO- https://raw.githubusercontent.com/dontbass/hlsproxy/main/install.sh | sh
set -e
REPO_URL="https://raw.githubusercontent.com/dontbass/hlsproxy/main"

case "$(uname -m)" in
	aarch64|arm64) BIN=hlsproxy_aarch64 ;;
	*) echo "Неподдерживаемая архитектура: $(uname -m)"; exit 1 ;;
esac

dl() { wget -qO "$2.new" "$REPO_URL/$1" && mv "$2.new" "$2"; }

echo "Загрузка $BIN..."
dl "bin/$BIN" /tmp/hlsproxy.bin
chmod +x /tmp/hlsproxy.bin
/tmp/hlsproxy.bin -version >/dev/null
[ -x /etc/init.d/hlsproxy ] && /etc/init.d/hlsproxy stop 2>/dev/null || true
mv /tmp/hlsproxy.bin /usr/bin/hlsproxy

dl init.d/hlsproxy /etc/init.d/hlsproxy
chmod +x /etc/init.d/hlsproxy
if [ -f /etc/config/hlsproxy ]; then
	# добавить новые опции, не трогая уже заданные
	for kv in mode=auto max_delay=180 max_pause=40 log_dir=/tmp/hlsproxy-log wan_access=0 ext_ui=0; do
		k=${kv%%=*}; v=${kv#*=}
		[ -n "$(uci -q get hlsproxy.main.$k)" ] || uci set hlsproxy.main.$k="$v"
	done
	uci commit hlsproxy
else
	dl files/hlsproxy.config /etc/config/hlsproxy
fi
mkdir -p /etc/hlsproxy
if [ -z "$(uci -q get hlsproxy.main.access_key)" ]; then
	uci set hlsproxy.main.access_key="$(tr -d '-' < /proc/sys/kernel/random/uuid)"
	uci commit hlsproxy
fi

mkdir -p /usr/lib/lua/luci/controller /usr/lib/lua/luci/view/hlsproxy
dl luci/controller/hlsproxy.lua /usr/lib/lua/luci/controller/hlsproxy.lua
dl luci/view/hlsproxy/index.htm /usr/lib/lua/luci/view/hlsproxy/index.htm
rm -rf /tmp/luci-indexcache* /tmp/luci-modulecache 2>/dev/null || true

/etc/init.d/hlsproxy enable
/etc/init.d/hlsproxy restart
IP=$(uci -q get network.lan.ipaddr | cut -d/ -f1); PORT=$(uci -q get hlsproxy.main.port || echo 8888)
echo "HLS Proxy $(/usr/bin/hlsproxy -version) установлен. Панель: http://${IP:-роутер}:$PORT/ui  (или LuCI → Службы → HLS Proxy)"
