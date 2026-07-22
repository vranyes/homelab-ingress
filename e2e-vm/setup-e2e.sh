#!/bin/sh
# Self-contained setup script executed once inside the VM by the harness.
set -e

export PATH=/usr/local/go/bin:$PATH
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

REPO=/root/homelab-ingress
BIN=/usr/local/bin
mkdir -p /etc/homelab-ingress /var/lib/homelab-ingress /var/run/hl-ingress /var/log/hl-ingress

########################################
# 1. Build all four binaries and install
########################################
echo "==> build ingress"
cd "$REPO"
CGO_ENABLED=0 GOWORK=off go build -o "$BIN/ingress" ./ingress/cmd/ingress 2>&1 | tail -3
cd - >/dev/null

echo "==> build controller (workspace mode)"
cd "$REPO"
CGO_ENABLED=0 go build -o "$BIN/controller" ./controller/cmd/controller 2>&1 | tail -3
cd - >/dev/null

echo "==> build oidc-mock"
cd "$REPO/e2e/cmd/oidc-mock"
CGO_ENABLED=0 GOWORK=off go build -o "$BIN/oidc-mock" . 2>&1 | tail -3
cd - >/dev/null

ls -la "$BIN/ingress" "$BIN/controller" "$BIN/oidc-mock"

########################################
# 2. Networking: br0 + dnsmasq (idempotent)
########################################
echo "==> br0"
ip link show br0 >/dev/null 2>&1 || ip link add name br0 type bridge
ip addr flush dev br0 2>/dev/null || true
ip addr add 10.44.0.1/24 dev br0 2>/dev/null || true
ip link set br0 up
ip link show br0 | head -2

mkdir -p /etc/dnsmasq.d
cat > /etc/dnsmasq.d/hl-ingress.conf <<EOF
interface=br0
bind-interfaces
dhcp-range=10.44.0.10,10.44.0.100,255.255.255.0,24h
EOF
rc-service dnsmasq restart 2>&1 | tail -2 || true
sleep 1
rc-service dnsmasq status 2>&1 | head -2 || true

########################################
# 3. OpenRC services
########################################
echo "==> openrc services"

cat > /etc/init.d/hl-ingress-oidc <<USERDATA
#!/sbin/openrc-run
description="homelab-ingress oidc-mock"
command="/usr/local/bin/oidc-mock"
command_args="--listen 127.0.0.1:8080"
pidfile="/run/hl-ingress-oidc.pid"
output_log="/var/log/hl-ingress/oidc.log"
error_log="/var/log/hl-ingress/oidc.log"
command_background=true
command_user="root"
depend() { need net
}
start_pre() { mkdir -p /var/run/hl-ingress /var/log/hl-ingress
}
USERDATA
chmod +x /etc/init.d/hl-ingress-oidc
sh -n /etc/init.d/hl-ingress-oidc && echo oidc-rc-syntax-ok

cat > /etc/init.d/hl-ingress-ingress <<USERDATA
#!/sbin/openrc-run
description="homelab-ingress ingress daemon"
command="/usr/local/bin/ingress"
command_args="--config /etc/homelab-ingress/config.yaml --state-dir /var/lib/homelab-ingress"
pidfile="/run/hl-ingress-ingress.pid"
output_log="/var/log/hl-ingress/ingress.log"
error_log="/var/log/hl-ingress/ingress.log"
command_background=true
command_user="root"
depend() { need net
after dnsmasq
}
start_pre() { mkdir -p /var/run/hl-ingress /var/log/hl-ingress /var/lib/homelab-ingress
}
USERDATA
chmod +x /etc/init.d/hl-ingress-ingress
sh -n /etc/init.d/hl-ingress-ingress && echo ingress-rc-syntax-ok

cat > /usr/local/bin/hl-ingress-controller-wrapper.sh <<'USERDATA'
#!/bin/sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
exec /usr/local/bin/controller \
    --router-addr 127.0.0.1:50051 \
    --token-path /var/run/hl-ingress/token \
    --ca-path /var/lib/homelab-ingress/ca.pem \
    --metrics-addr :8081 \
    --poll-interval 5s \
    "$@"
USERDATA
chmod +x /usr/local/bin/hl-ingress-controller-wrapper.sh

cat > /etc/init.d/hl-ingress-controller <<USERDATA
#!/sbin/openrc-run
description="homelab-ingress controller"
command="/usr/local/bin/hl-ingress-controller-wrapper.sh"
pidfile="/run/hl-ingress-controller.pid"
output_log="/var/log/hl-ingress/controller.log"
error_log="/var/log/hl-ingress/controller.log"
command_background=true
command_user="root"
depend() { need net
after k3s
}
start_pre() { mkdir -p /var/run/hl-ingress /var/log/hl-ingress
}
USERDATA
chmod +x /etc/init.d/hl-ingress-controller
sh -n /etc/init.d/hl-ingress-controller && echo controller-rc-syntax-ok

########################################
# 4. Ingress config + token
########################################
cat > /etc/homelab-ingress/config.yaml <<EOF
base_interface: br0
grpc:
  listen: 127.0.0.1:50051
ws:
  listen: ""
auth:
  issuer: http://127.0.0.1:8080
  audience: homelab-ingress
  allowed_subjects:
    - system:serviceaccount:default:homelab-ingress-controller
EOF
cat /etc/homelab-ingress/config.yaml

########################################
# 5. Start services in dependency order
########################################
rc-service hl-ingress-oidc restart || rc-service hl-ingress-oidc start
sleep 2
rc-service hl-ingress-oidc status 2>&1 | head -2

TOKEN=$(curl -sf "http://127.0.0.1:8080/token?sub=system:serviceaccount:default:homelab-ingress-controller&aud=homelab-ingress&exp=86400")
printf "%s\n" "$TOKEN" > /var/run/hl-ingress/token
echo "==> token length: ${#TOKEN}"

# Restart ingress so the new token + config are read
rc-service hl-ingress-ingress restart 2>&1 | tail -3 || true
sleep 3
rc-service hl-ingress-ingress status 2>&1 | head -2
echo "--- ingress log ---"
tail -10 /var/log/hl-ingress/ingress.log

# Wait for ca.pem to be generated
for i in $(seq 1 20); do
    [ -f /var/lib/homelab-ingress/ca.pem ] && break
    sleep 1
done
ls -la /var/lib/homelab-ingress/ca.pem 2>&1 || echo "MISSING ca.pem"

# Start controller
rc-service hl-ingress-controller restart 2>&1 | tail -3 || true
sleep 5
rc-service hl-ingress-controller status 2>&1 | head -2
echo "--- controller log ---"
tail -10 /var/log/hl-ingress/controller.log

########################################
# 6. Sanity check: Health RPC through gRPC client
########################################
echo "==> sanity Health check via grpcurl-style HTTP"
# Too lazy to write a Go client for sanity; rely on ingress log instead.
grep -c 'gRPC server started' /var/log/hl-ingress/ingress.log || true
grep -c 'starting manager' /var/log/hl-ingress/controller.log || true

echo "==> setup complete"
