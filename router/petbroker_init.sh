#!/bin/sh
# PetMarvel C1 本地 broker 开机启动（UCI firewall include 调用）
# 目标：设备零外网
#   - 1883(MQTT)  → 本机 broker
#   - 443(HTTPS)  → 本机假 HTTPS 服务端（应答阿里云 Link 的激活/OTA/日志）
#   - DNS         → 路由器 dnsmasq（本地应答），不产生外网流量
#   - 其余出站    → REJECT

i=0
while [ ! -f /data/petbroker_arm64.xz ]; do
    [ $i -gt 20 ] && exit 1
    i=$((i + 1))
    sleep 3
done

[ -x /tmp/petbroker ] || (xz -dc /data/petbroker_arm64.xz > /tmp/petbroker 2>/dev/null && chmod +x /tmp/petbroker)
[ -x /tmp/petbroker ] || exit 1

RUNNING=0
if [ -f /var/run/petbroker.pid ]; then
    kill -0 "$(cat /var/run/petbroker.pid)" 2>/dev/null && RUNNING=1
fi
if [ "$RUNNING" = "0" ]; then
    start-stop-daemon -S -b -x /tmp/petbroker -m -p /var/run/petbroker.pid \
        -- -dir /data -dev :1883 -ha :1884 -https :8443 -log /tmp/petbroker.log >/dev/null 2>&1 </dev/null
    sleep 2
fi

# ---- 按你的环境改这三项（也可以 export 覆盖）----
GW_IP="${GW_IP:-192.168.31.1}"          # 路由器自己的地址
DEV_HOSTNAME="${DEV_HOSTNAME:-tg7100c}" # 设备 DHCP hostname 里的特征串
DEV_OUI="${DEV_OUI:-00:11:22}"          # 设备 MAC 前缀，DHCP 查不到时兜底

# DNS 劫持（新设备首跳定位；老设备靠下面的 DNAT）
mkdir -p /tmp/dnsmasq.d
CONF=/tmp/dnsmasq.d/petmarvel.conf
WANT="address=/public.iot-as-mqtt.cn-shanghai.aliyuncs.com/$GW_IP"
if [ "$(cat $CONF 2>/dev/null)" != "$WANT" ]; then
    echo "$WANT" > $CONF
    DPID=$(ps | grep "[d]nsmasq" | head -1 | awk '{print $1}')
    [ -n "$DPID" ] && kill "$DPID" 2>/dev/null && sleep 3
fi

# 自动发现猫砂盆 IP
C1_IPS=$(awk -v h="$DEV_HOSTNAME" 'index($0,h) {print $3}' /tmp/dhcp.leases 2>/dev/null)
[ -z "$C1_IPS" ] && C1_IPS=$(awk -v m="$DEV_OUI" 'index($0,m) {print $3}' /tmp/dhcp.leases 2>/dev/null)
[ -z "$C1_IPS" ] && C1_IPS=$(awk -v m="$DEV_OUI" 'index($0,m) {print $1}' /proc/net/arp 2>/dev/null)
[ -z "$C1_IPS" ] && C1_IPS="192.168.31.100"

# DNAT 目标用的是路由器 LAN 地址，不依赖 route_localnet；
# 这一行保留只是为了兼容把 GW_IP 改成 127.0.0.1 的改法。
echo 1 > /proc/sys/net/ipv4/conf/br-lan/route_localnet 2>/dev/null

# 幂等清理（同时清掉历史上指向 127.0.0.1 的旧规则）
for ip in $C1_IPS; do
    for port in 1883 443; do
        p=$([ "$port" = "443" ] && echo 8443 || echo 1883)
        for dst in 127.0.0.1 "$GW_IP"; do
            while iptables -t nat -D PREROUTING -s "$ip" -p tcp --dport $port -j DNAT --to-destination "$dst":$p 2>/dev/null; do :; done
        done
    done
    while iptables -D FORWARD -s "$ip" -j REJECT --reject-with icmp-port-unreachable 2>/dev/null; do :; done
    while iptables -D FORWARD -s "$ip" -p udp --dport 53 -j ACCEPT 2>/dev/null; do :; done
    while iptables -D FORWARD -s "$ip" -p tcp --dport 53 -j ACCEPT 2>/dev/null; do :; done
    while iptables -D FORWARD -s "$ip" -p tcp --dport 443 -j ACCEPT 2>/dev/null; do :; done
done

# 铺规则：1883/443 全部转本机；FORWARD 只留 DNS
# 注意：DNAT 目标是路由器自己的 LAN 地址，不是 127.0.0.1。
# 转到 127.0.0.1 需要 route_localnet=1，而这是内核参数，重启/接口重载后会归零，
# 一旦归零 DNAT 就成了黑洞（设备连不上、白灯闪），所以这里刻意避开。
for ip in $C1_IPS; do
    iptables -t nat -A PREROUTING -s "$ip" -p tcp --dport 1883 -j DNAT --to-destination "$GW_IP":1883
    iptables -t nat -A PREROUTING -s "$ip" -p tcp --dport 443 -j DNAT --to-destination "$GW_IP":8443
    iptables -I FORWARD 1 -s "$ip" -p udp --dport 53 -j ACCEPT
    iptables -I FORWARD 2 -s "$ip" -p tcp --dport 53 -j ACCEPT
    iptables -I FORWARD 3 -s "$ip" -j REJECT --reject-with icmp-port-unreachable
done

iptables -C INPUT -i br-lan -p tcp --dport 1883 -j ACCEPT 2>/dev/null || \
    iptables -I INPUT -i br-lan -p tcp --dport 1883 -j ACCEPT
iptables -C INPUT -i br-lan -p tcp --dport 8443 -j ACCEPT 2>/dev/null || \
    iptables -I INPUT -i br-lan -p tcp --dport 8443 -j ACCEPT

echo "[petbroker] init done, c1 ips: $C1_IPS"
