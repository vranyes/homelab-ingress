#!/bin/sh
set -ex
echo "==> installing go 1.26.5"
curl -fsSL https://go.dev/dl/go1.26.5.linux-amd64.tar.gz -o /tmp/go.tar.gz
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tar.gz
rm /tmp/go.tar.gz

# Put go on the PATH for all shells (login + non-login).
echo 'export PATH="/usr/local/go/bin:$PATH"' > /etc/profile.d/go.sh
export PATH="/usr/local/go/bin:$PATH"
go version
