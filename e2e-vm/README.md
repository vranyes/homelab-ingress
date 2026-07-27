# e2e-vm — Lima VM for homelab-ingress end-to-end tests

This directory holds the provisioning definition for an Alpine Linux lima VM that
runs the full homelab-ingress stack: `ingress` (nftables + macvlan + DHCP data plane),
`oidc-mock` (local OIDC issuer), and the Kubernetes `controller` talking to a single-node
k3s cluster.
The VM is the isolated environment the Makefile's `e2e` target reuses to run the
end-to-end test suite.

## Files

| File              | Purpose                                                                                                                                                        |
|-------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `lima.yaml`       | Lima VM definition. The `provision:` block bootstraps the VM (apk packages, k3s install, br0 bridge, dnsmasq). The test harness spawns the three binaries.     |

## VM layout

```
qemu (x86_64, 4 vCPU, 4GiB RAM, 30GiB disk)
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

## VEPA hairpin relay

The ingress daemon creates each `lb-*` interface in **macvlan VEPA mode**
(`ingress/internal/macvlan/manager.go`, mirroring a real ISP link with a
hardware reflective-relay switch port). On a single host a VEPA child cannot
talk L2 to another interface in the *same* network stack: the Linux macvlan
driver drops frames whose source MAC reappears on the same stack, and a
one-port software bridge has no hairpin path. Without help, DHCP broadcasts
from `lb-xxxxx` would hit `hl-br0` and go nowhere; the e2e suite would hang
on `WaitForServiceStatusIP` for 60 seconds until the test timed out.

The relay fixes this by **moving the DHCP server into a separate network
namespace** (`ns-server`) and bouncing frames back with `tc mirred`:

1. A VEPA discover leaves `lb-xxxxx` → forwarded up to `hl-br0`.
2. `hl-br0` floods the broadcast out its only other port, `veth-inner`.
3. The frame arrives at `veth-outer` in `ns-server` (the "external switch
   port").
4. The `tc mirred egress mirror dev veth-outer` rule on the ingress qdisc
   copies the frame back out the same interface — i.e. it bounces a copy back
   across the veth pair to `hl-br0`. (`mirror`, not `redirect`: `redirect`
   steals the skb and skips local delivery, so dnsmasq's UDP socket never
   sees the broadcast. `mirror` lets dnsmasq receive it normally and ALSO
   re-injects a copy back toward `hl-br0`.) Because the re-injection happens
   in `ns-server`'s stack, the root-ns macvlan driver does not recognise the
   frame as a same-stack loop and does not drop it.
5. dnsmasq, living in `ns-server`, sees the broadcast on `veth-outer` and
   answers; the reply takes the inverse hairpin path back to `lb-xxxxx`.

This way the production code path (`MACVLAN_MODE_VEPA`) is exercised
end-to-end without modifying the Go source. Provisioning lives entirely in
`e2e-vm/lima.yaml` (see `/etc/init.d/hl-vepa-relay` and
`/etc/init.d/hl-dnsmasq-ns`); the e2e harness verifies it via
`harness.EnsureVepaRelayUp` at the start of `BeforeSuite`.

## Binaries and state

Lima is configured with `mounts: []` — there are no bind mounts.
The Makefile pushes artifacts in explicitly with `limactl copy`:

| Host binary                 | VM path                            |
|-----------------------------|------------------------------------|
| `ingress/cmd/ingress`       | `/usr/local/bin/ingress`           |
| `controller/cmd/controller` | `/usr/local/bin/controller`        |
| `oidc-mock`                 | `/usr/local/bin/oidc-mock`         |
| harness `config.yaml`       | `/etc/homelab-ingress/config.yaml` |

State lives entirely inside the VM and is wiped on `limactl delete`:

| Path                        | Contents                                                               |
|-----------------------------|------------------------------------------------------------------------|
| `/var/lib/homelab-ingress/` | `state.db`, `ca.pem`, `ca-key.pem` (ingress `--state-dir`)             |
| `/var/run/hl-ingress/`      | `token` — bearer token the controller sends to the ingress gRPC server |
| `/var/log/hl-ingress/`      | `ingress.log`, `oidc.log`, `controller.log`                            |
| `/etc/homelab-ingress/`     | `config.yaml` (config)                                                 |

## Managing services

The VM does **not** install OpenRC service definitions for the three
homelab-ingress binaries. Instead, the e2e test harness spawns them directly
as child processes in the correct dependency order:

| Binary                     | Args                                                                                             | Env             | Log file                        |
|----------------------------|--------------------------------------------------------------------------------------------------|-----------------|---------------------------------|
| `/usr/local/bin/oidc-mock` | _(none — defaults to `127.0.0.1:8080`)_                                                         | —               | `/var/log/hl-ingress/oidc.log`  |
| `/usr/local/bin/ingress`   | `--config /etc/homelab-ingress/config.yaml --state-dir /var/lib/homelab-ingress`                 | —               | `/var/log/hl-ingress/ingress.log` |
| `/usr/local/bin/controller` | `--router-addr 127.0.0.1:50051 --token-path /var/run/hl-ingress/token --ca-path /var/lib/homelab-ingress/ca.pem --metrics-addr :8081 --poll-interval 5s` | `KUBECONFIG=/etc/rancher/k3s/k3s.yaml` | `/var/log/hl-ingress/controller.log` |

The `BeforeSuite` in `e2e/suite_test.go` spawns them in order (oidc → ingress
→ controller), writes the JWT and config first, and waits for each to become
healthy before proceeding. `AfterSuite` terminates them in reverse order with
SIGTERM followed by SIGKILL after a 5-second grace period.

## How the Makefile interacts with this directory

A typical `make e2e` flow (lives in the repo-root Makefile, not here):

```sh
# 1. Build linux/amd64 binaries.
go build -o bin/ingress     ./ingress/cmd/ingress
go build -o bin/controller  ./controller/cmd/controller
go build -o bin/oidc-mock   ./oidc-mock/cmd/oidc-mock

# 2. Bring up the VM (idempotent — lima reuses an existing VM).
limactl start --name=hl-ingress-e2e ./e2e-vm/lima.yaml

# 3. Push binaries and config.
limactl copy bin/ingress                                 hl-ingress-e2e:/usr/local/bin/ingress
limactl copy bin/controller                              hl-ingress-e2e:/usr/local/bin/controller
limactl copy bin/oidc-mock                               hl-ingress-e2e:/usr/local/bin/oidc-mock
limactl copy e2e-vm/config.yaml                           hl-ingress-e2e:/etc/homelab-ingress/config.yaml

# 4. (Token minting and service startup happen inside the test suite, not here.)

# 5. Run the suite.
limactl shell hl-ingress-e2e -- go test ./e2e/...

# 6. Tail logs if something fails.
limactl shell hl-ingress-e2e -- tail -n200 /var/log/hl-ingress/*.log
```

The Makefile is the only thing that does `limactl copy` — `lima.yaml` has `mounts:
[]` on purpose so that a rebuild-and-retest does not silently pull in stale host-side
binaries through a bind mount.

## Tear-down

```sh
limactl stop  hl-ingress-e2e
limactl delete hl-ingress-e2e
```

The `qemu` VM type means no extra macOS daemons (socket_vmnet / vde_vmnet) are required;
the only host-side prerequisite is `lima` >= v0.22.
