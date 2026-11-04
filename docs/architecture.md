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
iptables -t nat -A PREROUTING -s <设备 IP> -p tcp --dport 1883 -j DNAT --to-destination 192.168.31.1:1883
iptables -t nat -A PREROUTING -s <设备 IP> -p tcp --dport 443  -j DNAT --to-destination 192.168.31.1:8443
```

只匹配**源**和**目的端口**，不看目的 IP，所以阿里云换节点也不影响。
设备 IP 从 DHCP 租约自动发现（hostname 里带 `tg7100c`），换了 IP 也能跟上。

**DNAT 目标必须是路由器自己的 LAN 地址，不要用 `127.0.0.1`。**
转到 `127.0.0.1` 要求 `net.ipv4.conf.br-lan.route_localnet=1`，而这是内核参数，
**重启或接口重载后会归零**；一旦归零，DNAT 就变成黑洞——规则看着还在，包全被当非法地址丢掉，
设备表现为连不上服务器（白灯闪）。用 LAN 地址走本地路由，没有这个依赖。

另外 `route_localnet` 归零时**规则依然存在**，所以"规则在 = 正常"这个判断会骗人，
排查时要看 `conntrack -L | grep <设备IP>` 有没有 ESTABLISHED。

### 2.5 规则要能跟着 DHCP 走

开机时 `/tmp/dhcp.leases` 往往还是空的（tmpfs，重启即空），脚本拿不到设备 IP 就会绑到兜底值，
之后设备就算在线也匹配不上。所以除了开机执行，还要挂一条 cron 定期重跑（脚本是幂等的）：

```
*/5 * * * * /data/petbroker_init.sh >/dev/null 2>&1
```

### 3. 假 HTTPS（封死最后的出网需求）

设备开机时会访问 `*.link.aliyuncs.com:443`（阿里云 Link 平台），其中最关键的是：

```
POST /auth/bootstrap
productKey=...&deviceName=...&clientId=...&timestamp=...&resources=mqtt
```

**设备是先用这个接口换到 `iotToken`，才去连 MQTT 1883 的。**
只回一个空的 200 JSON，设备拿不到 token 就永远不连 MQTT，表现就是白灯闪 + 每分钟重试一次。
broker 自己并不校验 token，所以回一个带 `iotToken` 的固定结构即可：

```json
{"code":200,"message":"success","data":{
  "iotId":"<deviceName>","iotToken":"<任意值>",
  "resources":{"mqtt":{"host":"public.iot-as-mqtt.cn-shanghai.aliyuncs.com","port":1883,"version":"1.0"}}}}
```

其余路径统一回 200 空 JSON，并把 method/URI/Host/UA/body 记进日志，方便以后按需定制。

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
