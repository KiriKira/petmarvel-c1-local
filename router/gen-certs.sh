#!/bin/sh
# 生成 broker 用的自签 RSA 证书。
# C1 固件不校验证书链，所以任意 CN 都能握手；这里用官方域名做 CN，
# 便于在抓包里对照，也避免某些客户端按 SNI 分流。
#
# 用法： sh gen-certs.sh [输出目录] [CN]
# 建议在 PC/容器里生成后把两个文件拷到路由器 /data/。
set -e

DIR="${1:-.}"
CN="${2:-public.iot-as-mqtt.cn-shanghai.aliyuncs.com}"
CNF="$(mktemp)"

cat > "$CNF" <<EOF
[req]
distinguished_name = dn
x509_extensions = v3
prompt = no
[dn]
CN = $CN
[v3]
subjectAltName = DNS:$CN
basicConstraints = CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
EOF

openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$DIR/broker.key" -out "$DIR/broker.crt" \
  -days 3650 -config "$CNF"

rm -f "$CNF"
chmod 600 "$DIR/broker.key"
echo "wrote $DIR/broker.crt"
echo "wrote $DIR/broker.key"
echo
echo "部署到路由器："
echo "  cat broker.crt | ssh root@<router> 'cat > /data/broker.crt'"
echo "  cat broker.key | ssh root@<router> 'umask 077; cat > /data/broker.key'"
