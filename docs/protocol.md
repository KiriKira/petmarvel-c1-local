# C1 设备协议（实测）

所有内容来自对自有设备 `PetMarvel C1`（productKey `a14KPydEhd3`，固件 1.0.2）的实际抓包与逐条回放验证。
不同固件版本可能有差异，遇到不一致时以设备实际回执为准。

## 传输层

- MQTT over TLS 到 `public.iot-as-mqtt.cn-shanghai.aliyuncs.com:1883`（IP 会变，走阿里云负载均衡）。
- **设备不校验服务端证书**：自签证书即可完成握手。这是本方案成立的前提。
- 设备 ClientHello 只包含 TLS 1.2 + RSA 密钥交换套件：

  ```
  0x002f TLS_RSA_WITH_AES_128_CBC_SHA
  0x0035 TLS_RSA_WITH_AES_256_CBC_SHA
  0x003c TLS_RSA_WITH_AES_128_CBC_SHA256
  0x003d TLS_RSA_WITH_AES_256_CBC_SHA256
  ```

  没有 `supported_versions` 扩展，也没有任何 ECDSA/ECDHE 套件。用 ECDSA 证书的中间 CA 会直接 `NO_SHARED_CIPHER`。
- Go 默认禁用了 RSA 密钥交换，broker 需要 `//go:debug tlsrsakex=1` 才能和它握手。

## MQTT 会话

- ClientId：`<productKey>.<deviceName>|securemode=2,tokenType=0,token=<TOKEN>,...`
- `deviceName` = 设备 MAC 去掉冒号并转小写（`00:11:22:33:44:55` → `001122334455`）。
  注意这与云端 `iotId`（形如 `wvJMG...`）不是一个东西：本地链路用 `deviceName`。
- keepalive 120s，QoS 0。
- 设备**不发送 SUBSCRIBE**。阿里云按 ClientId 里的 thing 身份路由系统 topic，自建 broker 同样按 ClientId 处理即可。

## Topic

| 方向 | Topic | 说明 |
|---|---|---|
| 云→设备 | `/sys/<pk>/<dn>/thing/model/down_raw` | 控制指令（TLV） |
| 设备→云 | `/sys/<pk>/<dn>/thing/model/up_raw` | 状态与回执（TLV） |
| 云→设备 | `/sys/<pk>/<dn>/thing/model/up_raw_reply` | 固定正文 `report reply` |
| 设备→云 | `/sys/<pk>/<dn>/thing/log/post` | JSON 日志 |
| 设备→云 | `/ext/ntp/<pk>/<dn>/request` | 校时请求 |
| 云→设备 | `/ext/ntp/<pk>/<dn>/response` | 校时响应 |

NTP 响应必须是字符串形式的毫秒时间戳：

```json
{"deviceSendTime":"<原样返回>","serverRecvTime":"<ms>","serverSendTime":"<ms>"}
```

不回或回错格式，设备会每秒重发，且记录时间全部漂移。

## down_raw 命令

TLV：`id(1B) len(1B) value(len B)`。

| hex | 含义 | 设备回执 |
|---|---|---|
| `01 01 01` | Clean now（清理，不换袋） | `11 01 01 13 01 00 01 01 01`，完成时 `11 01 02` |
| `01 01 03` | Empty litter / Dump（倒空整盆砂，必须换袋） | `11 01 07 ...` + `80 01 01 01 01 23` |
| `01 01 04` | Level litter（平砂） | `11 01 01 13 01 00 01 01 04` |
| `02 01 00/01` | 自动清理 | 原样回显 |
| `05 01 00/01` | 指示灯 | 原样回显 |
| `06 01 00/01` | 提示音 | 原样回显 |
| `07 01 00/01` | 童锁 / 按键锁 | 原样回显 |
| `0a 01 NN` | 延迟清理（NN=分钟） | 原样回显 |
| `10 05 <mask> <hh> <mm> 01 00` | 新增定时清理 | 原样回显 |
| `10 01 00` | 清空全部定时 | 无回执 |
| `14 01 00/01` | 幼猫模式 | `02 01 00 14 01 xx` |
| `15 01 00/01` | 自动埋砂 | 原样回显 |

定时清理的星期掩码实测 `0x7f` = 每天（bit0..6 对应周日..周六）。

## up_raw 字段

常规上报是 93 字节左右的完整快照，格式 `id(1B) len(1B) value(len B)` 循环。
增量变化时只发变化的字段。

| id | 含义 | 说明 |
|---|---|---|
| `0x01` | 事件码 | `0x1c`(28)=集便盒满，`0x23`(35)=告警 |
| `0x02` | 自动清理 | `00/01` |
| `0x04` | 猫在盆内 | `00/01` |
| `0x05` | 指示灯 | `00/01` |
| `0x06` | 提示音 | `00/01` |
| `0x07` | 童锁 | `00/01` |
| `0x09` | 最近一次使用记录 | 6 字节 = 3 个小端 uint16：`[未知][体重 g][时长 s]`，例 `11 00 95 12 6c 00` → 17 / 4757 / 108 |
| `0x0a` | 延迟清理分钟数 | |
| `0x0b` | 校准（Reset）完成标志 | |
| `0x0d` | 人体/猫接近检测 | |
| `0x11` | 工作状态 | 见下表 |
| `0x16` | 上盖在位 | **取反**：`00`=在位，`01`=取下（机械触点，3 秒内可能抖 4 次，需 ≥1s 去抖） |
| `0x17` | 集便盒在位 | **取反**：`00`=在位，`01`=抽出 |
| `0x18` | 集便盒已满 | `01`=满；抽出集便盒即清零 |
| `0x64` | 固件版本 | ASCII，如 `1.0.2` |
| `0x65` | 时间 | **两种语义**：完整帧里是秒偏移字符串（如 `"28800"`）；使用记录帧里是 `YYYY-MM-DD HH:MM:SS N` |
| `0x80` | 事件通道 | 常与 `0x01` 同时出现 |

### 0x11 工作状态

| 值 | 含义 |
|---|---|
| 00 | idle |
| 01 | cleaning |
| 02 | cleaning complete |
| 03 | dumping |
| 04 | dump complete |
| 05 | resetting |
| 06 | reset complete |
| 07 | paused |
| 08 | cat approaching（可恢复挂起，绿灯常亮） |
| 09 | cat entering（实测未出现，猫在盆内由 `0x04=01` 表达） |

实测未覆盖：`03`/`04`（需要真的倒砂全程）、`09`（未出现）。

## 设备行为要点

- 断电或重连后会先发 `/ext/ntp/.../request`，再发完整 up_raw 快照。
- 开机流程里会向 `*.link.aliyuncs.com:443` 发起 HTTPS 请求（激活 / OTA 检查 / 日志上报）。
  拦掉它设备会卡在开机状态反复重试，见 `docs/architecture.md` 里的假 HTTPS 说明。
- 状态上报是"变化触发 + 定期全量"，所以 HA 侧要接受**字段缺失**并保留上一次的值。
