# Home Assistant 补丁

本目录**不包含**完整的集成代码，只包含相对上游
[jarylc/hass-pet-marvel](https://github.com/jarylc/hass-pet-marvel) 的增量。
上游仓库没有 LICENSE 文件，因此这里以补丁形式分发自己的改动。

```
mqtt_client.py              新增：订阅本地 broker 的 MQTT 客户端
new_files/translations/     新增：简体中文翻译（上游只有 en.json）
patches/
  coordinator.py.patch      加 mqtt transport 分支、命令下发、状态映射
  config_flow.py.patch      加 Local broker 步骤 + 自动发现设备 + OptionsFlow 开关
  manifest.json.patch       改为 local_push、加 paho-mqtt 依赖、版本号
  sensor.py.patch           last_usage 的 0 值处理、错误状态映射
  translations_en.json.patch  新步骤/错误的英文文案
```

应用方法见 [../docs/ha-integration.md](../docs/ha-integration.md)。

## 改动要点

1. **transport 三态**：`options.use_local_broker` 为 True 走本地 MQTT，
   为 False 回云端，缺失则看 `entry.data["transport"]`。这样同一个 entry 可以在 UI 里切换。
2. **device_id 拆成两个**：实体 `unique_id` 继续用云端 `iotId`（避免实体重建丢历史），
   MQTT topic 用 `options["device_key"]`（= deviceName = MAC 去冒号）。
3. **自动发现**：config flow 里 Device ID 留空时，订阅 retained 的 `petmarvel/devices`
   读一次设备清单，渲染成下拉框。
4. **命令下发**：原集成通过阿里云 OpenAPI 改物模型属性，本地分支改成往
   `petmarvel/<dn>/cmd` 发 `{"cmd": "...", "value": ...}`。
