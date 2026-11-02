# PetMarvel C1 本地化

把 PetMarvel C1 猫砂盆从厂商云上摘下来，让家里的路由器直接当它的服务器。

- 设备**零外网**：MQTT 和开机时的 HTTPS 请求全部被接管，除了 DNS 查询不出任何流量
- **不需要 App**：控制与状态都在本地完成，App 可以卸载
- **Home Assistant 直连**：插件订阅本地 broker，保留一个开关随时切回云端
- **多设备**：新猫砂盆连上 WiFi 就自动被发现，DHCP 换 IP 也不影响

```
猫砂盆 ──TLS──▶ 路由器 petbroker ──MQTT──▶ Home Assistant
                  ├─ :1883  设备 MQTT（自签证书终结）
                  ├─ :8443  假 HTTPS（应答阿里云 Link 的激活/OTA/日志请求）
                  └─ :1884  HA 用的明文 MQTT
```

## 内容

```
broker/       Go 写的单文件 broker（静态编译，约 4 MB，xz 后 1.3 MB）
router/       路由器开机脚本 + 证书生成脚本
ha/           Home Assistant 补丁（新增 mqtt_client.py + 5 个 patch）
docs/         架构、协议表、部署、HA 接入、逆向过程
```

## 快速开始

1. **编译**（**必须用 Go 1.23.x**，见下）

   ```sh
   cd broker
   sh build.sh arm64          # 或 amd64 / mipsle / arm
   ```

   > ⚠️ Go 1.24 起彻底移除了 RSA 密钥交换（`GODEBUG tlsrsakex` 也一并移除），
   > 而 C1 固件只支持 `TLS_RSA_WITH_*` 套件。用新版 Go 编译出来的 broker 会在握手阶段
   > 直接失败。编译前先确认 `go version` 是 1.23.x。

2. **生成证书**

   ```sh
   sh router/gen-certs.sh ./out
   ```

3. **部署到路由器**：见 [docs/router-setup.md](docs/router-setup.md)

4. **接入 Home Assistant**：见 [docs/ha-integration.md](docs/ha-integration.md)

5. **想自己扩展协议**：见 [docs/protocol.md](docs/protocol.md)

## 已验证范围

| 项 | 状态 |
|---|---|
| 设备 TLS 终结 + MQTT 接入 | ✅ 实机长期运行 |
| 全部 7 个可写实体（Clean/Level/Dump + 4 开关） | ✅ |
| 状态字段（工作状态、上盖、集便盒、满盒、体重/时长、固件） | ✅ |
| 多设备并发、按设备路由命令 | ✅ 双设备模拟验证 |
| 开机自启 + 重启不丢配置 | ✅ UCI firewall include |
| 假 HTTPS 接管 443 | ✅ 服务端已验证，设备侧待其下次开机确认 |
| 工作状态 `03/04`（倒砂全程）、`09`（猫进入） | ⚠️ 未观测到 |

## 环境要求

- 路由器：OpenWrt 系，有 `iptables`、`conntrack`、`start-stop-daemon`、`xz`，
  可写持久分区（小米路由器的 `/data`），能 SSH
- Home Assistant：需要先装上游 [jarylc/hass-pet-marvel](https://github.com/jarylc/hass-pet-marvel)
- 设备：PetMarvel C1（productKey `a14KPydEhd3`），固件 1.0.2 验证通过

## 免责

- 仅用于**自己拥有**的设备，做的就是把自家设备指向自家服务器。
- 会让设备失去厂商 OTA 与云功能（这正是目的），固件更新需要自己想办法恢复。
- 协议细节按实测整理，不同固件版本可能不同，改动前先备份。
- 因使用本方案造成的设备异常、数据丢失、保修失效，自行承担。

## License

本仓库代码 MIT。Home Assistant 集成部分是上游项目的**补丁**，不含上游代码本身，
使用前请遵守上游项目条款。
