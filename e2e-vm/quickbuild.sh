#!/bin/sh
export PATH=/usr/local/go/bin:$PATH

cd /root/homelab-ingress/ingress
echo "==> building ingress"
go build -o /tmp/ingress-test ./cmd/ingress 2>&1 | tail -3
ls -la /tmp/ingress-test
