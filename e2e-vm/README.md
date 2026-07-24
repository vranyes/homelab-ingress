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
| `lima.yaml`       | Lima VM definition. The `provision:` block is the authoritative bootstrap (apk packages, k3s install, br0 bridge, dnsmasq, three OpenRC services).             |

## VM layout

```
qemu (x86_64, 4 vCPU, 4GiB RAM, 30GiB disk)
 |
 +-- eth0    NAT, k3s flannel interface, default route
 +-- br0     host-only bridge 10.44.0.1/24
              dnsmasq: 10.44.0.10-100 / 24h
              ingress attaches macvlan children here
```

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

## OpenRC services

The bootstrap installs three services under `/etc/init.d/` and adds them to the
`default` runlevel **but does not start them** — the test harness must start them
after writing `config.yaml` and the bearer token:

| Service                 | Binary / wrapper                                                                                        | Notes                                                                                                                                                                        |
|-------------------------|---------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `hl-ingress-oidc`       | `/usr/local/bin/oidc-mock`                                                                              | mock OIDC issuer; binding documented as `127.0.0.1:8080`                                                                                                                     |
| `hl-ingress-ingress`    | `/usr/local/bin/ingress --config /etc/homelab-ingress/config.yaml --state-dir /var/lib/homelab-ingress` | `//go:build linux`; uses nftables + macvlan + DHCP; gRPC on `127.0.0.1:50051` (per config)                                                                                   |
| `hl-ingress-controller` | `/usr/local/bin/hl-ingress-controller-wrapper.sh` → `/usr/local/bin/controller`                         | wrapper exports `KUBECONFIG=/etc/rancher/k3s/k3s.yaml` (OpenRC's `supervise-daemon` does not honour `command_user`'s environment for the spawned command, hence the wrapper) |

All three OpenRC units use `command_background=true` so OpenRC's `supervise-daemon`
supervises them and restarts on SIGKILL.
The `depend()` block on each unit pulls in `net`; the `ingress` unit also waits
for `dnsmasq`, and the `controller` unit waits for `k3s`.

## How the Makefile interacts with this directory

A typical `make e2e` flow (lives in the repo-root Makefile, not here):

```sh
# 1. Build linux/amd64 binaries.
go build -o bin/ingress     ./ingress/cmd/ingress
go build -o bin/controller  ./controller/cmd/controller
go build -o bin/oidc-mock   ./oidc-mock/cmd/oidc-mock   # or wherever it lives

# 2. Bring up the VM (idempotent — lima reuses an existing VM).
limactl start --name=hl-ingress-e2e ./e2e-vm/lima.yaml

# 3. Push binaries and config.
limactl copy bin/ingress                                 hl-ingress-e2e:/usr/local/bin/ingress
limactl copy bin/controller                              hl-ingress-e2e:/usr/local/bin/controller
limactl copy bin/oidc-mock                               hl-ingress-e2e:/usr/local/bin/oidc-mock
limactl copy e2e-vm/config.yaml                           hl-ingress-e2e:/etc/homelab-ingress/config.yaml

# 4. Mint a bearer token, push it.
limactl copy ./bin/token                                  hl-ingress-e2e:/var/run/hl-ingress/token

# 5. Start services and run the suite.
limactl shell hl-ingress-e2e -- sudo rc-service hl-ingress-oidc       start
limactl shell hl-ingress-e2e -- sudo rc-service hl-ingress-ingress   start
limactl shell hl-ingress-e2e -- sudo rc-service hl-ingress-controller start
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
