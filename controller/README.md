# homelab-ingress controller

Watches Services and EndpointSlices and instructs the `router` service
(`--router-addr` gRPC API) to update DHCP, interfaces, and nftables rules.

Quick start
- Build: `go build ./cmd/controller`
- Run locally: `./controller --router-addr <router-grpc-addr> --token-path <token-file> --ca-path <ca-pem>` (uses `KUBECONFIG` or in-cluster config)
- Flags: `--router-addr`, `--ws-event-addr`, `--poll-interval`, `--metrics-addr`, `--token-path`, `--ca-path` (see `cmd/controller/main.go`). Requires `--token-path`, `--ca-path`, non-empty `--router-addr`, positive `--poll-interval`.

Testing
- Run unit tests: `go test ./...` from the module root.

Layout
- `cmd/controller` - binary entrypoint
- `internal/` - implementation packages: `controller`, `router` (client), `config`

Notes
- Leader election is disabled; do not run two replicas against the same router.
- Health probe is hardcoded to `:8081`; keep `--metrics-addr` distinct.

Geo-blocking
- Set the annotation on a Service that uses the homelab-ingress class:

  ```yaml
  metadata:
    annotations:
      homelab-ingress.vranyes.com/allowed-geos: "US,CA"
  ```

- Codes are comma-separated ISO-3166 alpha-2, normalized (trim/uppercase/
  dedupe/drop invalid) and compared as a set; edits apply in place.
- `homelab-ingress.vranyes.com/allowed-cidrs` adds static IPv4 CIDRs
  (e.g. `"192.168.1.0/24"`) that are always allowed alongside the country
  list — e.g. so LAN clients are not locked out. Either annotation non-empty
  means deny-by-default with a union set. Invalid CIDRs keep the last-good CIDRs (none on first install)
  and still reconcile (fail closed, no hot-loop); IPv6 is rejected.
- Absent/empty (both annotations) means allow all. No CRD or extra RBAC needed.
- The daemon resolves countries to CIDRs. Requires `geoip.provider`;
  otherwise a non-empty geos list fails and sets no status IP (a CIDR-only
  allow-list works without it).

Rate limiting
- Cap new connections per source IP with two annotations:

  ```yaml
  metadata:
    annotations:
      homelab-ingress.vranyes.com/rate-limit: "5"        # connections/sec per client IP
      homelab-ingress.vranyes.com/rate-limit-burst: "10" # optional; defaults to the rate
  ```

- `rate-limit` is sustained rate, `rate-limit-burst` is capacity (defaults to
  rate). Missing `rate-limit` disables limiting; burst alone is ignored.
- Positive integers only; malformed values keep the existing limit (none on
  first install); rest still reconciles.
- Per Service, per source IP, new connections only, across all ports.

CA and TLS
- Verifies ingress with CA from `--ca-path` (`homelab-ingress-ca` at
  `/etc/homelab-ingress/ca.pem` in production).
- Only leaf cert regenerates on restart. To rotate CA, refresh the ConfigMap
  and restart the controller:

  ```sh
  kubectl -n default create configmap homelab-ingress-ca \
    --from-file=ca.pem=/path/to/ca.pem --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n default rollout restart deployment/homelab-ingress-controller
  ```
