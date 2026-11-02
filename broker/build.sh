#!/bin/sh
# 交叉编译 broker。
#
# 必须用 Go 1.23.x：Go 1.24 起彻底移除了 RSA 密钥交换（GODEBUG tlsrsakex 也一并移除），
# 而 C1 固件只支持 TLS_RSA_WITH_* 这类套件，用新版本编译出来的 broker 无法与设备握手。
#
# 用法： sh build.sh [arm64|amd64|mipsle|arm]
set -e

GO="${GO:-go}"
ARCH="${1:-arm64}"

ver="$($GO version | awk '{print $3}')"
case "$ver" in
  go1.23*|go1.22*|go1.21*) ;;
  *)
    echo "错误：需要 Go 1.23.x（或更早），当前是 $ver" >&2
    echo "Go 1.24+ 已移除 RSA 密钥交换，编译出的 broker 无法与设备握手。" >&2
    echo "下载： https://go.dev/dl/go1.23.4.linux-amd64.tar.gz" >&2
    exit 1
    ;;
esac

export CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH"
[ "$ARCH" = "arm" ] && export GOARM=7

$GO build -trimpath -ldflags "-s -w" -o "petbroker_$ARCH" .
xz -9 -c "petbroker_$ARCH" > "petbroker_$ARCH.xz"

echo "ok: petbroker_$ARCH ($(du -h "petbroker_$ARCH" | cut -f1)) -> $(du -h "petbroker_$ARCH.xz" | cut -f1)"
