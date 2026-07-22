#!/bin/sh
set -e
export PATH=/usr/local/go/bin:$PATH

REPO=/root/homelab-ingress

echo "==> build ingress"
cd "$REPO/ingress"
go build -o /usr/local/bin/ingress ./cmd/ingress 2>&1
cd - >/dev/null

echo "==> build controller"
cd "$REPO/controller"
go build -o /usr/local/bin/controller ./cmd/controller 2>&1
cd - >/dev/null

echo "==> build identicon"
cd "$REPO/e2e/cmd/identicon"
go build -o /usr/local/bin/identicon . 2>&1
cd - >/dev/null

echo "==> build oidc-mock"
cd "$REPO/e2e/cmd/oidc-mock"
go build -o /usr/local/bin/oidc-mock . 2>&1
cd - >/dev/null

ls -la /usr/local/bin/ingress /usr/local/bin/controller /usr/local/bin/identicon /usr/local/bin/oidc-mock
echo DONE
