# e2e-vm RUNBOOK — debugging the homelab-ingress e2e suite

All commands run **inside the VM** unless noted. Get there with:

```bash
make test-e2e-shell          # host: boots VM, stages binaries, drops into a root shell
# or, for a VM left behind by a failed run:
limactl shell hl-ingress-e2e
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
```

To re-run the suite (or a subset) by hand without rebooting the VM:

```bash
E2E_KEEP_VM=1 make test-e2e   # host: leaves the VM running afterwards
# inside the VM:
cd /root/homelab-ingress/e2e
go test -tags=e2e -v -run 'TwoLB|Masq' ./...
```

## Log locations

| Log | Meaning |
|---|---|
| `/var/log/hl-ingress/ingress.log` | ingress daemon (macvlan, DHCP, nftables, gRPC) |
| `/var/log/hl-ingress/controller.log` | k8s controller reconciles |
| `/var/log/hl-ingress/oidc.log` | mock OIDC issuer |
| `/var/log/dnsmasq-ns.log` | DHCP server (inside `ns-server`) |
| `/var/log/dnsmasq-ns.openrc.log` | dnsmasq service wrapper output |

`tail -n 200 /var/log/hl-ingress/*.log` is the fastest first step.

## Symptom → cause

### `WaitForServiceStatusIP` times out (60s)

The controller never wrote an IP. Check in order:

1. **Backends not Ready** (most common): `kubectl -n hl-ingress-e2e get pods` —
   `ImagePullBackOff` means the backend image isn't in k3s containerd.
   Fix: `k3s ctr images pull docker.io/traefik/whoami:latest`
   (provisioned automatically by `e2e-vm/scripts/04-backend-image.sh`).
2. **DHCP never acquired**: `tail /var/log/dnsmasq-ns.log` should show
   `DHCPDISCOVER`/`DHCPOFFER` per LB. If silent, verify the VEPA relay
   (next section).
3. **Controller stuck**: `grep -i "error\|unauthenticated\|invalid" /var/log/hl-ingress/controller.log`.
   `Unauthenticated` right after suite start points at the token file
   (`/var/run/hl-ingress/token`) or a restarted ingress with a new CA.

### Traffic dials `<nil>` or fails right after status appears

A stale `assigned_ip: "<nil>"` (pre-fix List bug) or a mid-convergence IP.
`kubectl -n hl-ingress-e2e get svc <name> -o jsonpath='{.status}'` — if the
IP looks wrong, wait one poll interval (5s) and re-check; the IP poller
corrects it once DHCP lands.

### `EnsureVepaRelayUp` fails in BeforeSuite

The failure message embeds a diagnostic dump. Manual checks:

```bash
ip netns list                          # must contain ns-server
ip -n ns-server -br addr               # veth-outer 10.44.0.2/24 UP
bridge link show dev veth-inner        # enslaved to hl-br0
tc -n ns-server filter show dev veth-outer ingress   # must say Egress Mirror
ip netns exec ns-server ss -lun '( sport = 67 )'     # dnsmasq on UDP/67
rc-service hl-vepa-relay restart && rc-service hl-dnsmasq-ns restart
```

### `Unauthenticated` in controller log

The controller's bearer token (`/var/run/hl-ingress/token`, minted in
`BeforeSuite`) doesn't validate: wrong audience, wrong subject, or the
ingress CA rotated under it (`/var/lib/homelab-ingress/ca.pem` mtime newer
than the test run). The daemon refuses to overwrite a valid CA (see
`ingress/internal/certgen`), so a fresh `ca.pem` means partial state was
found — restore `ca-key.pem` from backup.

## Inspecting data-plane state

```bash
ip link show type macvlan              # one lb-* interface per LB Service
nft --handle list table ip homelab_ingress   # per-service pre_* chains
k3s ctr images ls | grep whoami        # backend image cached?
kubectl -n hl-ingress-e2e get svc,deploy,pods -o wide
kubectl -n hl-ingress-e2e describe pod <name>   # ImagePullBackOff details
```

## Starting over

```bash
# host:
make remove-e2e-vm     # or: make test-e2e-down
make test-e2e          # clean boot + full suite
```
