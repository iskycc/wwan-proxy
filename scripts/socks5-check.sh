#!/bin/sh
# socks5-check.sh - 一键运行 SOCKS5 连通性测试（TCP CONNECT / BIND / UDP ASSOCIATE）
# 用法：
#   sh socks5-check.sh -proxy 23.141.204.202:59999 -user test -pass test123
#   sh socks5-check.sh -proxy 127.0.0.1:1080

set -u

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "[socks5-check] downloading test program..."
curl -fsSL https://raw.githubusercontent.com/iskycc/wwan-proxy/main/cmd/socks5-check/main.go -o "$TMPDIR/main.go" || {
  echo "[socks5-check] failed to download test program" >&2
  exit 1
}

cd "$TMPDIR"
go run main.go "$@"
