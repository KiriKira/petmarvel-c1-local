# 架构

## 目标

让猫砂盆**完全不依赖厂商云**：

- 控制和状态全部走家里的路由器
- App 不需要改指向、不需要常驻
- 设备不出任何外网流量（DNS 查询除外）
- Home Assistant 直接对接本地 broker

## 链路

```
                 ┌──────────────────────────── 路由器 192.168.31.1 ────────────────────────────┐
                 │                                                                             │
  猫砂盆 ──TLS──▶ │ PREROUTING                                                       petbroker   │
  192.168.31.100 │   ├─ tcp/1883 → DNAT 127.0.0.1:1883 ──────────────────────────▶ :1883 (TLS) │
                 │   ├─ tcp/443  → DNAT 127.0.0.1:8443 ──────────────────────────▶ :8443 (假 HTTPS)
                 │   └─ udp/53   → dnsmasq（本地应答）                                :1884 (明文 MQTT)
                 │        FORWARD: 仅放行 DNS，其余 REJECT                                        │
                 └──────────────────────────────────────────────────────┬──────────────────────┘
                                                                        │ petmarvel/# 
                                                              Home Assistant 插件
```

## 三层接管

### 1. DNS 劫持（只服务新设备）

```
address=/public.iot-as-mqtt.cn-shanghai.aliyuncs.com/192.168.31.1
```

设备解析服务器域名时直接得到路由器地址，于是主动连过来。
这一层只对**没有 DNS 缓存**的设备有意义——全新设备第一次配网、或者缓存过期之后。
已经在用的设备缓存着旧的阿里云 IP，DNS 改了对它没影响，所以还需要第二层。

### 2. DNAT（主力）

```
iptables -t nat -A PREROUTING -s <设备 IP> -p tcp --dport 1883 -j DNAT --to-destination 127.0.0.1:1883
iptables -t nat -A PREROUTING -s <设备 IP> -p tcp --dport 443  -j DNAT --to-destination 127.0.0.1:8443
echo 1 > /proc/sys/net/ipv4/conf/br-lan/route_localnet
```

只匹配**源**和**目的端口**，不看目的 IP，所以阿里云换节点也不影响。
设备 IP 从 DHCP 租约自动发现（hostname 里带 `tg7100c`），换了 IP 也能跟上。

`route_localnet` 必须有，否则 DNAT 到 `127.0.0.1` 会被内核当 martian 丢弃。

### 3. 假 HTTPS（封死最后的出网需求）

设备开机时会访问 `*.link.aliyuncs.com:443`（阿里云 Link 平台，做激活/OTA 检查/日志上报）。

**不能只 REJECT**：实测设备会卡在开机状态反复重试，永远不打开 MQTT。
所以 broker 内置一个 HTTPS 服务端，用同一张自签证书应答：

```
请求 → 记录 method/URI/Host/UA/body → 回 200 {"code":200,"success":true,...}
```

设备拿到成功响应就不再重试，也不需要真的出网。

## 为什么不用改 App

App 只是展示层。设备的控制和状态本来就是：

```
App → 阿里云 OpenAPI → MQTT down_raw → 设备
设备 → MQTT up_raw → 阿里云 → App 拉取
```

把 MQTT 这一段换成本地 broker 之后，设备行为不变、App 依然能看云端（如果它还联得上），
HA 则直接读本地 broker。要彻底摆脱 App，只要 HA 侧能下发全部命令——本方案已经覆盖
Clean / Level / Dump / 自动清理 / 自动埋砂 / 灯 / 音 / 童锁 / 幼猫模式 / 延迟 / 定时。

## 组件

| 组件 | 位置 | 职责 |
|---|---|---|
| `petbroker` | 路由器 `/tmp` | 终结设备 TLS、解析 TLV、维护每台设备状态、对外提供明文 MQTT（给 HA） |
| `petbroker_init.sh` | 路由器 `/data` | 开机解压启动 broker、重建劫持规则；由 UCI firewall include 调用 |
| HA 集成补丁 | Home Assistant | 把云轮询换成订阅本地 MQTT，保留云端开关可回退 |

## 多设备

broker 按 `deviceName` 维护独立会话，Topic 是：

- `petmarvel/devices` — 设备清单（retained）
- `petmarvel/<deviceName>/state` — 单设备状态
- `petmarvel/<deviceName>/cmd` — 下发命令

只有一台设备时，额外保留 `petmarvel/state` / `petmarvel/cmd` 兼容旧配置。

新设备接入只要满足两个条件之一：DNS 无缓存（走第一层），或它出现在 DHCP 租约里被自动发现（走第二层）。
