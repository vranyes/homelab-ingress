# homelab-ingress loadbalancer

The `loadbalancer` module implements the low-level system service that manages network interfaces, DHCP, macvlan, and nftables firewall rules and exposes a gRPC API consumed by the `controller` and CLI.

Overview
- Service: manages macvlan interfaces, DHCP clients, nftables firewall rules, and persists runtime state.
- API: gRPC `RouterService` defined in `pkg/api/router/v1` (see `proto/router/v1/router.proto`).
- Platform: linux-only; requires netlink and nftables access and typically runs as root.

Quick start
- Build: `go build ./cmd/loadbalancer`
- Run (default config): `sudo ./cmd/loadbalancer/loadbalancer --config /etc/homelab-lb/config.yaml`
- The service exposes a gRPC endpoint configured in the config file (default set in `cmd/loadbalancer`).

Testing
- Run unit tests: `go test ./...` from the module root.

Layout
- `cmd/loadbalancer` - binary entrypoint
- `internal/` - implementation packages: `dhcp`, `firewall`, `macvlan`, `recovery`, `service`, `state`
- `pkg/api/router/v1` - generated protobuf + gRPC code used by clients

Notes
- This module uses the `//go:build linux` tag and interacts with kernel networking primitives; it should be deployed on a Linux host with appropriate capabilities.
