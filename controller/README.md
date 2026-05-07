# homelab-ingress controller

The `controller` module implements a Kubernetes controller that watches Services and Pods and coordinates with the `router` service to configure networking and firewall rules for workloads.

Overview
- Purpose: reconcile Kubernetes Service/Pod state and instruct the `router` service to update DHCP, interfaces, and nftables rules.
- Integrations: connects to the Kubernetes API (controller-runtime) and to the `router` gRPC API (`--router-addr`).

Quick start
- Build: `go build ./cmd/controller`
- Run locally: `./cmd/controller/controller --router-addr <router-grpc-addr> ~/.kube/config`
- Flags: `--router-addr`, `--poll-interval`, `--metrics-addr`, (see `cmd/controller/main.go`).

Testing
- Run unit tests: `go test ./...` from the module root.

Layout
- `cmd/controller` - binary entrypoint
- `internal/` - implementation packages: `controller`, `router` (client), `config`

Notes
- Uses `sigs.k8s.io/controller-runtime` and supports leader election when running in-cluster.
