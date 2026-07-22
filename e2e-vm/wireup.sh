#!/bin/sh
# Sequential setup: br0, dnsmasq, openrc scripts, ingress config, token, services.
set -e

mkdir -p /etc/homelab-ingress /var/lib/homelab-ingress /var/run/hl-ingress /var/log/hl-ingress

echo "==> hl-br0"
ip link show hl-br0 >/dev/null 2>&1 || ip link add name hl-br0 type bridge
ip addr flush dev hl-br0 2>/dev/null || true
ip addr add 10.44.0.1/24 dev hl-br0 2>/dev/null || true
ip link set hl-br0 up

# Disable bridge-netfilter for hl-br0 so k3s iptables rules don't filter
# DHCP traffic between macvlan children and dnsmasq.
echo 0 > /sys/class/net/hl-br0/bridge/nf_call_iptables 2>/dev/null || true
echo 0 > /sys/class/net/hl-br0/bridge/nf_call_arptables 2>/dev/null || true

mkdir -p /etc/dnsmasq.d
cat > /etc/dnsmasq.d/hl-ingress.conf <<EOF
interface=hl-br0
bind-interfaces
dhcp-range=10.44.0.10,10.44.0.100,255.255.255.0,24h
EOF
rc-service dnsmasq restart 2>&1 | tail -2 || true
sleep 1

echo "==> write openrc scripts"
# /etc/init.d/hl-ingress-oidc
cat > /etc/init.d/hl-ingress-oidc <<EORC
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
EORC
chmod +x /etc/init.d/hl-ingress-oidc

# /etc/init.d/hl-ingress-ingress
cat > /etc/init.d/hl-ingress-ingress <<EORC
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
EORC
chmod +x /etc/init.d/hl-ingress-ingress

# Controller wrapper
cat > /usr/local/bin/hl-ingress-controller-wrapper.sh <<'EORC'
#!/bin/sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
exec /usr/local/bin/controller \
    --router-addr 127.0.0.1:50051 \
    --token-path /var/run/hl-ingress/token \
    --ca-path /var/lib/homelab-ingress/ca.pem \
    --metrics-addr :8081 \
    --poll-interval 5s \
    "$@"
EORC
chmod +x /usr/local/bin/hl-ingress-controller-wrapper.sh

# /etc/init.d/hl-ingress-controller
cat > /etc/init.d/hl-ingress-controller <<EORC
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
EORC
chmod +x /etc/init.d/hl-ingress-controller

# Syntax check
sh -n /etc/init.d/hl-ingress-oidc && echo oidc-rc-ok
sh -n /etc/init.d/hl-ingress-ingress && echo ingress-rc-ok
sh -n /etc/init.d/hl-ingress-controller && echo controller-rc-ok

# ingress config
cat > /etc/homelab-ingress/config.yaml <<EOF
base_interface: hl-br0
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

echo "==> start oidc-mock"
rc-service hl-ingress-oidc restart 2>&1 | tail -3 || rc-service hl-ingress-oidc start
sleep 2
rc-service hl-ingress-oidc status 2>&1 | head -2

echo "==> mint token"
TOKEN=$(curl -sf "http://127.0.0.1:8080/token?sub=system:serviceaccount:default:homelab-ingress-controller&aud=homelab-ingress&exp=86400")
printf "%s" "$TOKEN" > /var/run/hl-ingress/token
echo "token length: ${#TOKEN}"

echo "==> start ingress"
rc-service hl-ingress-ingress restart 2>&1 | tail -3 || rc-service hl-ingress-ingress start
sleep 3
rc-service hl-ingress-ingress status 2>&1 | head -2
echo "--- ingress log tail ---"
tail -10 /var/log/hl-ingress/ingress.log

# Wait for ca.pem
for i in $(seq 1 20); do
    [ -f /var/lib/homelab-ingress/ca.pem ] && break
    sleep 1
done
ls -la /var/lib/homelab-ingress/ca.pem 2>&1

echo "==> start controller"
rc-service hl-ingress-controller restart 2>&1 | tail -3 || rc-service hl-ingress-controller start
sleep 5
rc-service hl-ingress-controller status 2>&1 | head -2
echo "--- controller log tail ---"
tail -10 /var/log/hl-ingress/controller.log

echo "==> setup complete"
