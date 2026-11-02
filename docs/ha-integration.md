# Home Assistant 接入

## 前置

先安装上游集成 [jarylc/hass-pet-marvel](https://github.com/jarylc/hass-pet-marvel)（HACS 或手动），
本仓库**只提供本地链路相关的补丁**，不复刻整个集成——上游没有 LICENSE，直接转发不合规。

补丁基于上游 `custom_components/pet_marvel/`，如果上游结构有大改动，`patch` 可能失败，
按下面的说明手工合并即可。

## 步骤

```sh
cd /config/custom_components/pet_marvel

# 1) 新增本地 MQTT 客户端
cp <repo>/ha/mqtt_client.py .

# 2) 打补丁（-p2 对应 patch 里的 a/custom_components/pet_marvel/... 前缀）
patch -p2 < <repo>/ha/patches/coordinator.py.patch
patch -p2 < <repo>/ha/patches/config_flow.py.patch
patch -p2 < <repo>/ha/patches/manifest.json.patch
patch -p2 < <repo>/ha/patches/sensor.py.patch
patch -p2 < <repo>/ha/patches/translations_en.json.patch

# 3) 中文翻译（上游没有这个文件）
cp <repo>/ha/new_files/translations/zh-Hans.json translations/

# 4) 清缓存并重启
rm -rf __pycache__
ha core restart
```

## 配置

在 UI 里添加集成时选 **Local broker**：

- Broker host：路由器地址（如 `192.168.31.1`）
- Broker port：`1884`
- Device ID：**留空** → 会自动列出 broker 上报的所有设备供选择

也可以事后在集成的 **配置 → 选项** 里切换 `Use local broker`：

- 打开 → 完全走本地 MQTT
- 关闭 → 回到原来的云端轮询

## 关键点：Device ID 用 deviceName，不是 iotId

| | 值 | 用途 |
|---|---|---|
| `iotId` | `wvJMG...`（云端长 ID） | 原集成的 `device_id`，走阿里云 OpenAPI 时用 |
| `deviceName` | MAC 去冒号小写，如 `001122334455` | **本地 MQTT topic 用这个** |

迁移老条目时，为了让实体 `unique_id` 不变（否则所有实体会被重建、历史丢失），
补丁里把两者分开了：

- 实体前缀继续用原来的 `device_id`
- MQTT topic 用 `options["device_key"]`

所以改 `.storage/core.config_entries` 时要同时给 `options` 加上：

```json
{"use_local_broker": true, "mqtt_host": "192.168.31.1", "mqtt_port": 1884, "device_key": "001122334455"}
```

## 验证

1. 日志里应该有（默认 logger 会吞掉 INFO，需要临时把级别调成 debug）：

   ```yaml
   logger:
     logs:
       custom_components.pet_marvel: debug
   ```

   ```
   Setting up pet_marvel
   petmarvel mqtt connect rc=0 topic=petmarvel/001122334455/state
   Finished fetching ... (success: True)
   ```

2. 实体应该有值：`status` / `bin_full` / `bin_inserted` / `lid_installed` /
   `auto_clean` / `error_status` 等。

3. 点一下 `button.*_clean`，broker 日志应出现 `[ha] cmd petmarvel/<dn>/cmd {"cmd":"clean"}`，
   设备随后上报 `11 01 01`。

## 多台设备

一台设备一个 entry：再添加一次集成，同样把 Device ID 留空，从列表里选另一台。
broker 的 `petmarvel/devices` 会带上每台的 `device_id` / `ip` / `online` / `fw_version`。

## 常见问题

**实体 unavailable** = broker 上还没有这台设备的状态（设备离线或没劫持成功）。
集成会以 `ConfigEntryNotReady` 重试，这属于正常行为。

**所有实体都变成 unavailable 且日志报 unique_id 冲突** = 直接改 `device_id` 导致实体重建，
按上面「关键点」用 `options.device_key` 区分。

**`last_usage` 显示 1970** = `0x65` 被当成了时间串。补丁里已处理：只有含 `:` 的才解析成时间，
`"28800"` 这类秒偏移写进 `last_usage_raw`。
