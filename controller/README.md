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

Geo-blocking
- Set the annotation on a Service that uses the homelab-ingress class:

  ```yaml
  metadata:
    annotations:
      homelab-ingress.vranyes.com/allowed-geos: "US,CA"
  ```

- The controller parses comma-separated ISO-3166 alpha-2 codes (trim, uppercase,
  de-duplicate, drop invalid), forwards them as `allowed_geos` on
  `CreateLoadBalancer`/`UpdateLoadBalancer`, and compares them as a set on every
  reconcile. Annotation edits are applied without recreating the load balancer.
- `GetLoadBalancer`/`ListLoadBalancers` return the router's current list, which
  the controller uses for drift detection.
- An absent or empty annotation means allow all. There is no CRD and no extra
  RBAC/env: the existing Service watch is sufficient.
- The ingress daemon resolves countries to CIDRs; the controller never handles
  IP ranges. If the daemon has no `geoip.provider` configured, a non-empty list
  fails with `InvalidArgument` and the Service does not receive a status IP.
