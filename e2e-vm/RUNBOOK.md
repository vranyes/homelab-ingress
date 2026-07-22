# E2E VM Runbook

**Purpose:** One-shot reproducer + debug guide so any agent can bring up the full
homelab-ingress e2e stack and run the Ginkgo suite without tribal knowledge.

---

## Architecture

```
macOS / Linux host
  └── Lima VM (Alpine 3.22 linux-virt, 4 vCPU, 4 GiB RAM)
        ├── k3s (v1.36, flannel on eth0, no traefik/servicelb)
        │     └── kubernetes API  (127.0.0.1:6443)
        │
        ├── hl-ingress-oidc   (oidc-mock, 127.0.0.1:8080, openrc)
        │     └── JWKS + RS256 token mint for ingress auth
        │
        ├── hl-ingress-ingress (ingress daemon, gRPC 127.0.0.1:50051, openrc)
        │     ├── macvlan children on br0 (10.44.0.0/24)
        │     ├── nftables DNAT chains  (homelab-lb-*)
        │     └── dnsmasq DHCP leases  (10.44.0.10–100)
        │
        └── hl-ingress-controller (controller, k8s watcher, openrc)
              ├── --router-addr 127.0.0.1:50051  (gRPC to ingress)
              ├── --token-path /var/run/hl-ingress/token
              ├── --metrics-addr 127.0.0.1:9080  (avoids :8080 conflict)
              └── --kubeconfig /etc/rancher/k3s/k3s.yaml
```

**Key invariant:** All four processes (k3s, oidc-mock, ingress, controller)
run inside the same VM as plain OS processes supervised by openrc. The test
binary itself also runs inside the VM via `limactl exec ... go test`.

**Networking:** `eth0` is Lima's SLIRP/NAT interface. `br0` is a host-only
bridge (10.44.0.1/24) created by the provision script. Pod traffic uses eth0
(via flannel). LB traffic uses br0 via macvlan children. k3s API traffic stays
on the localhost socket. No `socket_vmnet` or `lima-guestagent` bridging needed.

---

## Quick Reproducer

```bash
# One command builds the VM, starts everything, runs all 10 specs:
make test-e2e
```

### Step-by-step (manual debug flow)

```bash
# 1. Boot the VM (idempotent — safe to re-run)
limactl start --name=hl-ingress-e2e ./e2e-vm/lima.yaml

# 2. Copy the repo in (tar via stdin — no permission issues on /root)
limactl shell hl-ingress-e2e -- sudo mkdir -p /root/homelab-ingress
tar czf - --no-xattrs -C . . \
  | limactl shell hl-ingress-e2e -- sudo tar xzf - -C /root/homelab-ingress/

# 3. Wait for k3s to be healthy
limactl shell hl-ingress-e2e -- sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml \
  kubectl wait --for=condition=Ready node --all --timeout=120s

# 4. Build all 4 binaries (persist in /usr/local/bin so they survive reboots)
limactl shell hl-ingress-e2e -- sudo PATH=/usr/local/go/bin:$PATH sh -c '
  cd /root/homelab-ingress
  CGO_ENABLED=0 go build -o /usr/local/bin/ingress    ./ingress/cmd/ingress
  CGO_ENABLED=0 go build -o /usr/local/bin/controller ./controller/cmd/controller
  CGO_ENABLED=0 go build -o /usr/local/bin/oidc-mock  ./e2e/cmd/oidc-mock
  CGO_ENABLED=0 go build -o /usr/local/bin/identicon  ./e2e/cmd/identicon
'

# 5. Wire up config, token, and start all services (order matters)
limactl shell hl-ingress-e2e -- sudo PATH=/usr/local/go/bin:$PATH sh -c '
  set -e
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

  # Restart dnsmasq to pick up any config changes
  rc-service dnsmasq restart

  # Start oidc-mock first (ingress needs it for its token verifier)
  rc-service hl-ingress-oidc start
  sleep 1

  # Ingress generates CA + config on first boot; re-use existing state
  if [ ! -f /etc/homelab-ingress/config.yaml ]; then
    rc-service hl-ingress-ingress start
    sleep 3
    rc-service hl-ingress-ingress stop
  fi

  # Start ingress
  rc-service hl-ingress-ingress start
  sleep 2

  # Start controller
  rc-service hl-ingress-controller start
  sleep 3

  echo "Services started:"
  rc-service --list | grep hl-
'

# 6. Run the suite
limactl shell hl-ingress-e2e -- sudo PATH=/usr/local/go/bin:$PATH sh -c '
  cd /root/homelab-ingress/e2e
  go test -tags=e2e -timeout=25m -v ./...
'

# 7. Tear down (optional)
make test-e2e-down
```

---

## Common Debug Commands

### VM lifecycle
```bash
limactl list                              # show VM state
limactl stop hl-ingress-e2e               # graceful stop (wipes /tmp, keeps /usr/local/bin)
limactl start hl-ingress-e2e              # resume (runs provision script again, idempotent)
limactl delete hl-ingress-e2e             # destroy and reclaim disk
```

### Service control
```bash
# Status
limactl shell hl-ingress-e2e -- sudo rc-status

# Start / restart / stop
limactl shell hl-ingress-e2e -- sudo rc-service hl-ingress-oidc start
limactl shell hl-ingress-e2e -- sudo rc-service hl-ingress-ingress restart
limactl shell hl-ingress-e2e -- sudo rc-service hl-ingress-controller stop

# Verify processes are running
limactl shell hl-ingress-e2e -- sudo ps aux | grep -E 'ingress|controller|oidc-mock' | grep -v grep
```

### Logs
```bash
limactl shell hl-ingress-e2e -- sudo tail -f /var/log/hl-ingress/ingress.log
limactl shell hl-ingress-e2e -- sudo tail -f /var/log/hl-ingress/controller.log
limactl shell hl-ingress-e2e -- sudo tail -f /var/log/hl-ingress/oidc.log
limactl shell hl-ingress-e2e -- sudo tail -f /var/log/k3s.log
```

### k3s
```bash
limactl shell hl-ingress-e2e -- sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml \
  kubectl get nodes -o wide
limactl shell hl-ingress-e2e -- sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml \
  kubectl get pods -A
```

### Network state (inside VM)
```bash
limactl shell hl-ingress-e2e -- sudo ip addr show         # eth0 + br0
limactl shell hl-ingress-e2e -- sudo ip link show         # br0 exists
limactl shell hl-ingress-e2e -- sudo nft list ruleset      # homelab-lb-* chains
limactl shell hl-ingress-e2e -- sudo cat /var/lib/dnsmasq/dnsmasq.leases  # DHCP
```

### gRPC connectivity test
```bash
limactl shell hl-ingress-e2e -- sudo PATH=/usr/local/go/bin:$PATH sh -c '
  cd /root/homelab-ingress
  cat <<EOF | go run ./e2e/cmd/oidc-mock &
package main
func main() {}
EOF
  # Instead, just verify the ingress gRPC is listening:
  ss -tlnp | grep 50051
'
```

### Test binary connectivity (from inside VM)
```bash
limactl shell hl-ingress-e2e -- sudo PATH=/usr/local/go/bin:$PATH sh -c '
  cd /root/homelab-ingress/e2e
  # Dry-run the test binary (compile only):
  go test -tags=e2e -c -o /tmp/e2e.test ./
  # List all specs without running:
  /tmp/e2e.test --ginkgo.focus="" --ginkgo.dry-run --ginkgo.v
'
```

---

## Known Issues and Lessons Learned

### ⚠️ `lima0` vs `eth0` — CRITICAL
Lima 1.x named the SLIRP interface `lima0`. Lima 2.x renamed it to `eth0`.
Using `--flannel-iface=lima0` with k3s on a Lima 2.x VM causes k3s to fail
with `lima0 does not exist`. Fix: always use `--flannel-iface=eth0`. The lima.yaml
provision script now hard-codes this. Verify with:
```bash
limactl shell hl-ingress-e2e -- ip addr show eth0
```

### ⚠️ OpenRC heredoc whitespace — CRITICAL
When embedding openrc init scripts in the Lima YAML block scalar, the content
is indented 6 spaces. OpenRC's shebang `#!/sbin/openrc-run` MUST have no leading
whitespace, otherwise openrc returns `Exec format error`. Fix in lima.yaml:
use `tee /path/to/script <<'EOF' | sed 's/^      //'` to strip the YAML indent.
The original `cat > file <<'EOF'` approach with 6-space-indented content caused
all three services to fail to start on first boot.

### ⚠️ Controller metrics port conflict — CRITICAL
The controller defaults to `--metrics-addr :8080`. The oidc-mock also binds
`127.0.0.1:8080`. The wrapper script in `/usr/local/bin/hl-ingress-controller-wrapper.sh`
must pass `--metrics-addr 127.0.0.1:9080` to avoid this collision.
Verify: `ss -tlnp | grep 9080` shows controller metrics on the VM.

### ⚠️ `limactl copy` permission denied on `/root` — WORKAROUND
`limactl copy` drops files as root into `/root/`. If the directory permissions
are too restrictive (drwx------), copy fails with "Permission denied". The
Makefile workaround uses `tar | sudo tar` instead of `limactl copy`. This is
handled in `_e2e-vm-boot`; do not switch back to `limactl copy` without fixing
the `/root` permissions first:
```bash
limactl shell hl-ingress-e2e -- sudo chmod 755 /root
```

### ⚠️ `ca-certificates` must be installed before k3s install
The k3s install script (`curl -sfL https://get.k3s.io`) makes an HTTPS request.
On Alpine, if `ca-certificates` is not installed, the SSL handshake fails and
the script aborts (Alpine's wget prints a certificate error and `set -eu` aborts).
The lima.yaml provision script installs `ca-certificates` before k3s.

### ⚠️ Go 1.26 required — Alpine ships Go 1.24
`apk add go` on Alpine 3.22 installs Go 1.24, but this project requires Go 1.26.
The provision script downloads `go1.26.5.linux-amd64.tar.gz` from `go.dev/dl`
and installs it to `/usr/local/go`. Always use `PATH=/usr/local/go/bin:$PATH`
when invoking `go` inside the VM.

### ⚠️ Binaries in `/tmp/` are wiped on reboot
`/tmp/` and `/var/log/hl-ingress/` are wiped on every `limactl stop`. The openrc
service scripts (`/etc/init.d/hl-ingress-*`) persist across reboots. Binaries
installed to `/usr/local/bin/` also persist. The Makefile installs binaries to
`/usr/local/bin/` so they survive `limactl stop/start` cycles.

### ⚠️ `wireup.sh` must be idempotent — use `if [ ! -f ]` guards
The provision script in lima.yaml runs on every `limactl start`. Any file writes
must guard with `if [ ! -f ]` to avoid overwriting state on reboot. The openrc
service scripts and the controller wrapper script both use this guard pattern.

### ⚠️ `limactl shell VM -- cmd` quoting on macOS (zsh)
`limactl shell VM -- somecmd with args` can cause zsh to swallow metacharacters
like `&` or `|`. Workaround: write the command to `/tmp/script.sh`, `limactl copy`
it, and invoke with `limactl shell VM -- sh /tmp/script.sh`. The Makefile avoids
this by using `sudo sh -c '...'`.

### ⚠️ `make test-e2e` vs `make test-e2e-shell` — no parallel runs
If `make test-e2e` is running and a second invocation starts, the AfterSuite from
the first run calls `rc-service hl-ingress-controller stop` (and the other
services), killing the processes that the second BeforeSuite expects to be
running. Always wait for `make test-e2e` to finish, or use `E2E_KEEP_VM=1` to
leave the VM up after a run.

### ⚠️ Backend container image (`traefik/whoami`) — KNOWN GAP
`DeployIdenticon` (harness/k8s.go) creates a k8s Deployment using the public
`traefik/whoami` image. The Lima VM's k3s runs with its own embedded containerd
that has no internet access — it cannot pull from Docker Hub. Pods fail with
`ImagePullBackOff`.

Consequently the following 7 specs fail at `WaitForDeploymentReady`:
- `one_lb` (1 target, 3 targets)
- `two_lb` (both specs)
- `churn` (re-create step: 0→1 targets)

**To fix permanently:** pre-pull `traefik/whoami` into k3s's containerd:
```bash
limactl shell hl-ingress-e2e -- sudo PATH=/usr/local/go/bin:$PATH sh -c '
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  # Pull the image using ctr (k3s embedded containerd, not docker)
  ctr images pull docker.io/library/traefik/whoami:latest
'
```

The 3 specs that don't need a backend Deployment work fully:
`zero_lb`, `oidc_reject`, `controller_restart`.

### ✅ What Works

- VM boots cleanly from `limactl start` with idempotent provision script
- k3s becomes healthy with `Ready` node within 60s of first boot
- All 3 openrc services start successfully with correct dependency ordering
- Controller log shows `starting manager`, `metrics server on 127.0.0.1:9080`,
  `Starting workers` (service controller active)
- Ingress log shows `gRPC server started with TLS listen=127.0.0.1:50051`
- Controller log shows `DeleteLoadBalancer` calls when services are deleted
- Ginkgo suite runs all 10 specs (`Ran 10 of 10 Specs`)
- `zero_lb_test.go` passes: `Health.active_macvlans == 0`, clean nftables
- `sharedK8s()` / `sharedRouter()` correctly share state from `BeforeSuite`

### 📋 Scenario Status

| Spec | Status | Notes |
|------|--------|-------|
| zero_lb | ✅ Pass | macvlan=0, clean nftables, no errors |
| one_lb 0tgt | ✅ Pass | no backend Deployment needed |
| one_lb 1tgt | ⚠️ ImagePullBackOff | k3s cannot pull traefik/whoami from Docker Hub |
| one_lb 3tgt | ⚠️ ImagePullBackOff | k3s cannot pull traefik/whoami from Docker Hub |
| two_lb 1+3 | ⚠️ ImagePullBackOff | k3s cannot pull traefik/whoami from Docker Hub |
| two_lb shared-80 | ⚠️ ImagePullBackOff | k3s cannot pull traefik/whoami from Docker Hub |
| churn del | ✅ Pass | no backend needed for delete phase |
| churn rec | ⚠️ ImagePullBackOff | traefik/whoami image needed on recreate |
| oidc_reject | ✅ Pass | Token mutation → Unauthenticated, no status IP |
| controller_restart | ✅ Pass | pkill -9 → openrc respawns, round-robin resumes |

**Net: 5/10 passing. Fix the traefik/whoami image pull to unlock the remaining 5.**

---

## Lima YAML Provision Script Tips

The provision script in `e2e-vm/lima.yaml` runs as root in a separate shell for
each `mode: system` item. Key patterns used:

```bash
# Idempotent file creation (won't overwrite on reboot)
if [ ! -f /etc/init.d/my-service ]; then
  tee /etc/init.d/my-service <<'EOF' | sed 's/^      //'
  #!/sbin/openrc-run
  ...
EOF
  chmod +x /etc/init.d/my-service
  rc-update add my-service default
fi

# Idempotent service start
rc-service my-service start   # safe even if already running
rc-service my-service restart  # safe even if not started

# Go version check (fails with set -eu if go not found)
if [ ! -x /usr/local/go/bin/go ] || \
   [ "$(/usr/local/go/bin/go version | awk '{print $3}')" != "go1.26.5" ]; then
  ...
fi

# Profile script (survives reboot, sourced by login shells)
printf '%s\n' 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/golang.sh
```

---

## AfterSuite Behavior

`AfterSuite` stops all three openrc services. Ginkgo always runs `AfterSuite`,
even if a spec panics. This means running `make test-e2e` twice in quick
succession without an intervening `limactl stop` causes the second run to find
services dead. Fix: always `limactl stop` between runs, or use `E2E_KEEP_VM=1`
and explicitly `rc-service hl-ingress-{oidc,ingress,controller} start` to
resume.

---

## CI Integration

`.github/workflows/e2e.yaml` installs Lima, caches the image, and runs
`make test-e2e`. On Linux CI hosts, `limactl` may require systemd or socket
activation; follow the Lima Linux install guide. The same `make test-e2e` target
works on both macOS and Linux CI.