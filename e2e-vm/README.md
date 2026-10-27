# e2e-vm — Lima VM for homelab-ingress end-to-end tests

This directory holds the provisioning definition for an Alpine Linux lima VM that
runs the full homelab-ingress stack: `ingress` (nftables + macvlan + DHCP data plane),
`oidc-mock` (local OIDC issuer), and the Kubernetes `controller` talking to a single-node
k3s cluster.
The VM is the isolated environment the Makefile's `test-e2e` target reuses to run the
end-to-end test suite. Host-side prerequisite: `lima` >= 2.0 (per
`minimumLimaVersion` in `lima.yaml`). `lima.yaml` defines the VM; its
`provision:` block bootstraps packages, k3s, bridge, dnsmasq, and the backend
pre-pull. The e2e test binary (running in-VM) spawns the three binaries
during `BeforeSuite`.

## VM layout

```
VM (4 vCPU, 4GiB RAM, 30GiB disk; qemu/x86_64 on Intel, vz/aarch64 on Apple Silicon)
 |
 +-- eth0    NAT, k3s flannel interface, default route
 +-- hl-br0  host-only bridge 10.44.0.1/24  (root netns)
       ├── lb-xxxxx  macvlan VEPA child (one per LB Service; created by ingress)
       └── veth-inner ──veth pair──┐
                                     │
                          ns-server (netns)
                            veth-outer 10.44.0.2/24
                            dnsmasq bound to veth-outer (DHCP 10.44.0.10-100 / 24h)
                            tc qdisc ingress on veth-outer
                              filter: match all → action mirred egress mirror self
```

## Geo mock database

`scripts/03-geolite-mock.sh` writes `/var/lib/homelab-ingress/mock-geo.tsv`,
mapping the bridge subnet's four `/26` slices to US, CA, EU and XX for
`geoip.provider: mock`. The suite rewrites the file in `BeforeSuite`, and geo
specs that need an EU-mapped source add the secondary address `10.44.0.129/24`
to `veth-outer` inside `ns-server`.

## VEPA hairpin relay

Each `lb-*` interface is macvlan VEPA (like a real ISP reflective-relay port).
On one host a VEPA child cannot reach the same stack — DHCP from `lb-*`
would die on `hl-br0`, hanging the suite on `WaitForServiceStatusIP`.

The fix: dnsmasq lives in a separate netns (`ns-server`), and a `tc mirred`
rule bounces frames back across the veth pair:

1. Discover leaves `lb-xxxxx` → `hl-br0` floods it to `veth-inner` →
   arrives at `veth-outer` in `ns-server`.
2. `tc mirred egress mirror dev veth-outer` re-injects a copy toward `hl-br0`
   (`mirror` keeps a local copy for dnsmasq; re-injection from `ns-server`
   avoids the same-stack drop).
3. dnsmasq answers on `veth-outer`; the reply takes the inverse path.

Provisioning is in `lima.yaml` (`/etc/init.d/hl-vepa-relay`,
`hl-dnsmasq-ns`); `harness.EnsureVepaRelayUp` verifies the relay in
`BeforeSuite`. (VEPA mode itself is exercised by the ingress macvlan manager.)

## Binaries and state

Lima is configured with `mounts: []` — there are no bind mounts.
The Makefile stages artifacts explicitly with `tar | limactl shell ... tar xzf -`
(see `copy-e2e-vm` / `copy-ingress-vm` in the root Makefile):

| Host artifact               | VM path                            |
|-----------------------------|------------------------------------|
| ingress binary              | `/usr/local/bin/ingress`           |
| controller binary           | `/usr/local/bin/controller`        |
| oidc-mock binary            | `/usr/local/bin/oidc-mock`         |
| generated `config.yaml`     | `/etc/homelab-ingress/config.yaml` |

Sources stream to `/root/homelab-ingress/` the same way, run in-VM as root
(`copy-e2e-vm` stages `e2e/` + `controller/`; `copy-ingress-vm` stages
`ingress/` for in-VM unit tests).

State lives entirely inside the VM and is wiped on `limactl delete`:

| Path                        | Contents                                                               |
|-----------------------------|------------------------------------------------------------------------|
| `/var/lib/homelab-ingress/` | `state.db`, `ca.pem`, `ca-key.pem` (ingress `--state-dir`)             |
| `/var/run/hl-ingress/`      | `token` — bearer token the controller sends to the ingress gRPC server |
| `/var/log/hl-ingress/`      | `ingress.log`, `oidc.log`, `controller.log`                            |
| `/etc/homelab-ingress/`     | `config.yaml` (config)                                                 |

## Managing services

No OpenRC services for the three test binaries (bridge/relay/dnsmasq are
OpenRC services) — the harness spawns binaries in dependency order:

| Binary                     | Args                                                                                             | Env             | Log file                        |
|----------------------------|--------------------------------------------------------------------------------------------------|-----------------|---------------------------------|
| `/usr/local/bin/oidc-mock` | _(none — defaults to `127.0.0.1:8080`)_                                                         | —               | `/var/log/hl-ingress/oidc.log`  |
| `/usr/local/bin/ingress`   | `--config /etc/homelab-ingress/config.yaml --state-dir /var/lib/homelab-ingress`                 | —               | `/var/log/hl-ingress/ingress.log` |
| `/usr/local/bin/controller` | `--router-addr 127.0.0.1:50051 --token-path /var/run/hl-ingress/token --ca-path /var/lib/homelab-ingress/ca.pem --metrics-addr :18080 --poll-interval 5s` | `KUBECONFIG=/etc/rancher/k3s/k3s.yaml` | `/var/log/hl-ingress/controller.log` |

The `BeforeSuite` in `e2e/suite_test.go` starts oidc → ingress → controller
(JWT and config first, waiting for each to become healthy); `AfterSuite`
reverses with SIGTERM/SIGKILL.

## How the Makefile interacts with this directory

A typical `make test-e2e` flow (lives in the repo-root Makefile, not here):

```sh
make build-e2e-binaries  # cross-build for the VM (GOOS=linux, host-arch GOARCH)
make ensure-e2e-vm       # boot the VM if needed (idempotent)
make copy-e2e-vm         # stage sources + binaries; config is suite-generated
make test-e2e            # ensure + copy + go test in-VM; deletes VM afterwards
                         # unless E2E_KEEP_VM is set
limactl shell hl-ingress-e2e -- sudo tail -n200 /var/log/hl-ingress/*.log
```

Staging uses `tar | limactl shell` only, so retest cannot pick up stale host
binaries.

## Tear-down

```sh
make remove-e2e-vm     # or: make test-e2e-down (alias)
```
