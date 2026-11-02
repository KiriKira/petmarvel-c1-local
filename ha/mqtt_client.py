"""本地 MQTT 客户端：与路由器上的 petbroker 交互（多设备）。

topic 约定：
- petmarvel/devices          设备清单（retain，broker 自动维护）
- petmarvel/<device>/state   单设备状态（retain）
- petmarvel/<device>/cmd     下发命令
单设备时代的老 topic（petmarvel/state、petmarvel/cmd）仍作兼容。
"""

from __future__ import annotations

import json
import logging
import threading

import paho.mqtt.client as mqtt

_LOGGER = logging.getLogger(__name__)

DEVICES_TOPIC = "petmarvel/devices"
LEGACY_STATE_TOPIC = "petmarvel/state"
LEGACY_CMD_TOPIC = "petmarvel/cmd"


class C1MQTTClient:
    """最小 MQTT 客户端：订阅本设备状态 + 设备清单，发布本设备命令。"""

    def __init__(self, host: str, port: int = 1884, device_id: str = "") -> None:
        self.host = host
        self.port = int(port)
        self.device_id = device_id
        if device_id:
            self.state_topic = f"petmarvel/{device_id}/state"
            self.cmd_topic = f"petmarvel/{device_id}/cmd"
        else:
            self.state_topic = LEGACY_STATE_TOPIC
            self.cmd_topic = LEGACY_CMD_TOPIC

        self._state: dict = {}
        self._devices: list = []
        self._connected = False
        self._lock = threading.Lock()
        self._client = mqtt.Client(client_id=f"ha-petmarvel-{device_id or 'c1'}")
        self._client.on_connect = self._on_connect
        self._client.on_disconnect = self._on_disconnect
        self._client.on_message = self._on_message

    # ---------- lifecycle ----------
    def connect(self) -> None:
        """阻塞式建立连接，随后开启后台网络线程。"""
        self._client.connect(self.host, self.port, keepalive=60)
        self._client.loop_start()

    def close(self) -> None:
        try:
            self._client.loop_stop()
            self._client.disconnect()
        except Exception:  # pragma: no cover - 关闭阶段容错
            pass

    # ---------- paho callbacks ----------
    def _on_connect(self, client, userdata, flags, rc, properties=None) -> None:
        self._connected = rc == 0
        if self._connected:
            client.subscribe(self.state_topic)
            client.subscribe(DEVICES_TOPIC)
        _LOGGER.debug("petmarvel mqtt connect rc=%s topic=%s", rc, self.state_topic)

    def _on_disconnect(self, client, userdata, rc, properties=None) -> None:
        self._connected = False

    def _on_message(self, client, userdata, msg) -> None:
        if msg.topic == self.state_topic:
            try:
                data = json.loads(msg.payload.decode("utf-8"))
            except Exception:
                _LOGGER.warning("petmarvel mqtt: 无法解析 state payload")
                return
            with self._lock:
                self._state = data
        elif msg.topic == DEVICES_TOPIC:
            try:
                listed = json.loads(msg.payload.decode("utf-8"))
            except Exception:
                return
            if isinstance(listed, list):
                with self._lock:
                    self._devices = listed

    # ---------- api ----------
    @property
    def connected(self) -> bool:
        return self._connected

    def get_state(self) -> dict:
        with self._lock:
            return dict(self._state)

    def get_devices(self) -> list:
        with self._lock:
            return list(self._devices)

    def send_command(self, cmd: str, value=None) -> None:
        payload: dict = {"cmd": cmd}
        if value is not None:
            payload["value"] = value
        self._client.publish(self.cmd_topic, json.dumps(payload))
        _LOGGER.debug("petmarvel mqtt cmd -> %s: %s %s", self.cmd_topic, cmd, value)


def discover_devices(host: str, port: int, timeout: float = 5.0) -> list:
    """同步探测 broker：连上后读一次 retain 的设备清单（给 config flow 用）。"""
    result: dict = {"list": []}
    done = threading.Event()

    def on_connect(client, userdata, flags, rc, properties=None):
        if rc == 0:
            client.subscribe(DEVICES_TOPIC)
        else:
            done.set()

    def on_message(client, userdata, msg):
        if msg.topic != DEVICES_TOPIC:
            return
        try:
            listed = json.loads(msg.payload.decode("utf-8"))
        except Exception:
            listed = []
        result["list"] = listed if isinstance(listed, list) else []
        done.set()

    client = mqtt.Client(client_id="ha-petmarvel-discover")
    client.on_connect = on_connect
    client.on_message = on_message
    try:
        client.connect(host, int(port), keepalive=30)
    except Exception as err:
        _LOGGER.debug("petmarvel discover connect failed: %s", err)
        return []
    client.loop_start()
    done.wait(timeout)
    client.loop_stop()
    try:
        client.disconnect()
    except Exception:
        pass
    return result["list"]
