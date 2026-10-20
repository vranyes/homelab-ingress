#!/bin/sh
set -ex
echo "==> installing go 1.26.5"
# Match the guest architecture: the VM is aarch64 on Apple Silicon (vz) and
# x86_64 in CI (qemu). Installing linux-amd64 into an aarch64 guest works only
# via Rosetta and makes every in-VM build several times slower.
case "$(uname -m)" in
  aarch64|arm64) GOARCH=arm64 ;;
  x86_64|amd64)  GOARCH=amd64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
curl -fsSL "https://go.dev/dl/go1.26.5.linux-${GOARCH}.tar.gz" -o /tmp/go.tar.gz
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tar.gz
rm /tmp/go.tar.gz

# Put go on the PATH for all shells (login + non-login).
echo 'export PATH="/usr/local/go/bin:$PATH"' > /etc/profile.d/go.sh
export PATH="/usr/local/go/bin:$PATH"
go version
