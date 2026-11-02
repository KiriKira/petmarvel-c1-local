# 路由器部署

适用于 OpenWrt 系（本文在小米路由器上验证：aarch64、musl、根分区只读且基本写满）。

## 1. 交叉编译

```sh
cd broker
sh build.sh arm64        # 输出 petbroker_arm64 + petbroker_arm64.xz（约 1.3 MB）
```

MIPS/ARMv7 路由器：`sh build.sh mipsle` / `sh build.sh arm`。

> **必须用 Go 1.23.x**。Go 1.24 起移除了 RSA 密钥交换，而 C1 只有 `TLS_RSA_WITH_*` 套件。
> `build.sh` 会先检查版本，不匹配直接退出。

## 2. 生成证书

```sh
sh router/gen-certs.sh ./out
```

## 3. 传到路由器

dropbear 通常没有 `sftp-server`，`scp` 会失败，用管道：

```sh
cat petbroker_arm64.xz | ssh root@192.168.31.1 'cat > /data/petbroker_arm64.xz'
cat out/broker.crt      | ssh root@192.168.31.1 'cat > /data/broker.crt'
cat out/broker.key      | ssh root@192.168.31.1 'umask 077; cat > /data/broker.key'
```

老款小米路由器的 SSH 只提供 `ssh-rsa` 主机密钥，新版 OpenSSH 需要：

```sh
ssh -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa root@192.168.31.1
```

## 4. 首次手动启动，验证

```sh
ssh root@192.168.31.1 '
  xz -dc /data/petbroker_arm64.xz > /tmp/petbroker && chmod +x /tmp/petbroker
  start-stop-daemon -S -b -x /tmp/petbroker -m -p /var/run/petbroker.pid \
    -- -dir /data -dev :1883 -ha :1884 -https :8443 -log /tmp/petbroker.log \
    >/dev/null 2>&1 </dev/null
  sleep 2
  tail -5 /tmp/petbroker.log
'
```

看到这三行就说明端口起来了：

```
device TLS listening on :1883
HA MQTT listening on :1884
fake HTTPS listening on :8443
```

## 5. 开机自启

Root 分区基本写满，`/data` 才是持久分区。用 UCI firewall include 注册启动脚本
（比 `rc.local` 可靠，这是 ShellCrash 的做法）：

```sh
cat router/petbroker_init.sh | ssh root@192.168.31.1 'cat > /data/petbroker_init.sh; chmod 755 /data/petbroker_init.sh'

ssh root@192.168.31.1 '
  uci set firewall.petbroker=include
  uci set firewall.petbroker.type=script
  uci set firewall.petbroker.path=/data/petbroker_init.sh
  uci commit firewall
'
```

脚本做的事：等 `/data` 挂载 → 解压二进制 → pidfile 判活后启动 → 重建 DNS 劫持 /
DNAT / FORWARD 规则（幂等，可反复执行）。

## 6. 验证接管

```sh
ssh root@192.168.31.1 '
  # 设备在连谁：dst 是公网 IP 但回程 src=127.0.0.1 → DNAT 生效
  conntrack -L | grep <设备 IP> | grep 1883
  # 规则
  iptables -t nat -S PREROUTING | grep 1883
  iptables -L FORWARD -n --line-numbers | head
  # broker 是否收到设备
  tail -20 /tmp/petbroker.log
'
```

日志里出现下面这行即接入成功：

```
[device] CONNECT clientId=<pk>.<dn>|... -> device=<dn> product=<pk>
[device] up_raw 0201...
```

## 常见问题

**设备白灯闪** = WiFi 没连上。检查 DNAT 还在不在、broker 是否在监听（规则在但没人接，设备会白闪）。

**设备一直不连** = 先看 `iptables -L FORWARD -n -v` 有没有被 REJECT 的计数。
如果 443 被拒，设备会卡在开机阶段（见 `docs/architecture.md`）。

**`scp` 报错** = dropbear 没 sftp-server，用上面 `cat | ssh 'cat >'`。

**进程跟着 SSH 会话死掉** = 少了 `>/dev/null 2>&1 </dev/null`。
`start-stop-daemon -b` 仍然会继承 stdout，不重定向的话进程会被挂住或在会话结束时被杀。

**重启后规则没了** = UCI include 没提交（`uci commit firewall`），或者脚本没执行权限。

**`/data` 空间不够** = `xz -9` 压缩后约 1.3 MB；可以把二进制换成 `-ldflags "-s -w"` 再压。
