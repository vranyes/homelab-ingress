# Geoblocking: design and as-built notes

Per-LoadBalancer country filtering for homelab-ingress. Operators attach a list
of ISO-3166 alpha-2 country codes to a Service; the ingress daemon resolves
them to IPv4 CIDRs and enforces the list in nftables before DNAT.

This document began as the implementation plan. It was rewritten after the
feature shipped so it describes the code as it exists. The original plan is in
git history (before commit `b65e00c`); the design decisions are recorded in
ADRs 0001–0006.

## Status

Implemented and covered by unit and e2e tests:

- protobuf + controller plumbing (`allowed_geos`)
- `ingress/internal/geoip` (MaxMind GeoLite2 and DB-IP Lite providers)
- `ingress/internal/geo` (database watcher, service→CIDR map, rebuild callback)
- nftables deny rule and per-service union sets
- controller Service annotation
- four e2e specs, including a live annotation flip and a database hot reload

## Data flow

```text
Service annotation
  homelab-ingress.vranyes.com/allowed-geos: "US,CA"
            │
            ▼
controller: AllowedGeos() → Create/Update RPC (allowed_geos)
            │
            ▼
ingress gRPC: normalize + validate ISO-3166 alpha-2
            │
            ▼
state.LBService.AllowedGeos (persisted in state.json)
            │
            ▼
nftables AddRules: resolve countries → per-service union set geo_<sn>
                   emit deny rule first, then DNAT, then final drop
            │
            ▼
geoip timer/watcher reloads the database
            │
            ▼
geo.Manager.OnRebuild → nftables.RefreshGeoSets (flush + repopulate sets)
```

No country data is consulted at packet time. The kernel performs one radix-tree
membership lookup per packet.

## Kubernetes interface

The annotation lives on the `Service`, not on an Ingress object. The Service
must use the homelab-ingress load balancer class:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: web
  annotations:
    homelab-ingress.vranyes.com/allowed-geos: "US,CA"
spec:
  type: LoadBalancer
  loadBalancerClass: homelab-ingress
  selector:
    app: web
  ports:
    - name: http
      port: 80
      targetPort: 8080
```

Remove the annotation to allow all traffic:

```bash
kubectl annotate svc web homelab-ingress.vranyes.com/allowed-geos-
```

Parser semantics (`controller/internal/controller/annotations.go`):

- comma-separated codes
- trimmed, uppercased, de-duplicated (`" us, ca,US "` → `["US","CA"]`)
- invalid entries are dropped
- absent, empty, or all-invalid → `nil` (allow all)

> **Operational sharp edge:** a typo such as `USA` normalises to no codes, so
> the Service becomes unrestricted. The ingress validates the format again but
> cannot guess the intended country. Treat the annotation as security-relevant
> configuration and review it like one.

## gRPC interface

`proto/router/v1/router.proto`:

```proto
message CreateLoadBalancerRequest {
  string service_key = 1;
  repeated PortMapping ports = 2;
  repeated string allowed_geos = 3; // ISO-3166 alpha-2
}

message UpdateLoadBalancerRequest {
  string service_key = 1;
  repeated PortMapping ports = 2;
  repeated string allowed_geos = 3;
}

message GetLoadBalancerResponse {
  string service_key = 1;
  string assigned_ip = 2;
  repeated PortMapping ports = 3;
  bool dhcp_acquired = 4;
  string interface_name = 5;
  repeated string allowed_geos = 6;
}
```

Controller client:

```go
Create(ctx, serviceKey string, ports []PortMapping, geos []string) (*ServiceEntry, error)
Update(ctx, serviceKey string, ports []PortMapping, geos []string) error
```

`Get`/`List` populate `ServiceEntry.AllowedGeos`, which lets the controller
compare desired state (annotation) with the router's actual state.

The controller reconciles when ports or geos change:

```go
geos := AllowedGeos(svc)
// Create when the LB does not exist, otherwise:
if portsChanged(existing.Ports, ports) || geosChanged(existing.AllowedGeos, geos) {
    r.RouterClient.Update(ctx, serviceKey, ports, geos)
}
```

`geosChanged` compares as a set, so reordering the annotation does not churn
the router. Annotation-only edits arrive through the normal Service watch.

## Providers and configuration

The config block and Go package are named `geoip` because the backend is
pluggable:

```yaml
geoip:
  provider: dbip            # maxmind | dbip | mock; omit to disable
  # license_key: ...        # required only for provider: maxmind
  # download_url: ...       # optional mirror/offline override
  refresh_interval: 24h
  stale_after: 840h
  # cache_path: /var/lib/homelab-ingress/dbip-country-lite.mmdb
```

| provider | key | source | format | refresh |
|---|---|---|---|---|
| `maxmind` | required | GeoLite2 Country | `.tar.gz` containing an MMDB | every `refresh_interval` (24h default) |
| `dbip` | none | DB-IP IP-to-Country Lite | `.mmdb.gz` (raw gzip) | month-aware daily check; 35d stale default |
| `mock` | none | local TSV fixture | TSV | n/a (e2e only) |

Defaults:

- `cache_path`: `<state-dir>/geolite2-country.mmdb`, `<state-dir>/dbip-country-lite.mmdb`,
  or `<state-dir>/mock-geo.tsv`.
- `stale_after`: 7d for MaxMind, 35d for DB-IP.
- A bare `license_key` with no provider implies `maxmind`; `license_key: "mock"`
  still selects the mock backend.
- An empty `provider` disables geo-blocking entirely. A daemon with no provider
  rejects non-empty `allowed_geos` with `InvalidArgument`.

DB-IP Lite is free but licensed CC BY 4.0. The daemon logs
`geo data by DB-IP (https://db-ip.com)` at startup when selected; keep that
attribution in operator-facing material.

Both real providers download to a temp file, `fsync`, verify the MMDB metadata
marker, and rename atomically. A failed download leaves the previous cache and
reader intact.

## nftables enforcement

For a service with a non-empty allow-list, the per-service chain is:

```text
1. ip daddr <extIP> ip saddr not in @geo_<sn> counter drop
2. one DNAT rule per target (protocol + port)
3. ip daddr <extIP> counter drop
```

The deny rule **must** come first: an `accept` verdict in nat prerouting
terminates chain evaluation and would skip DNAT (ADR 0001).

- Set name: `geo_<sn>`, where `sn = safeName(serviceKey)`.
- One interval set per service, containing the union of all allowed countries'
  IPv4 CIDRs.
- Existing sets are flushed and repopulated in place; the deny rule's `SetID`
  stays valid, so no chain rebuild is needed on a database reload (ADR 0004).
- The existing `RemoveRules` cleanup deletes sets whose name contains `sn`.
- An unknown country (valid alpha-2, no CIDRs in the database) logs a warning,
  contributes no CIDRs, and keeps the deny rule — the service **fails closed**.
- Services without `allowed_geos` keep the original DNAT-then-drop shape.

## Reload path

Everything happens in-process; there is no cron job or restart:

1. `main.go` starts `geoip.Manager.Run` and `geo.Manager.Run` as goroutines.
2. The provider manager downloads on its interval or month boundary.
3. The atomic rename fires the fsnotify watcher (plus a 30s metadata poll as a
   fallback), which calls `geo.Manager.Reload`.
4. `geo.Manager` reopens the MMDB and builds the per-service CIDR map from
   `state.Clone()`.
5. `OnRebuild` hands that map to `nftables.Manager.RefreshGeoSets`, which
   flushes and repopulates each `geo_<sn>` set.

There is currently **no on-demand refresh trigger** (no SIGHUP or admin RPC).
Updates occur on the timer or when the cache file changes. A SIGHUP handler is
the smallest future addition if manual refreshes are needed.

## Failure semantics

| Situation | Behaviour |
|---|---|
| No `geoip.provider` | daemon runs; `allowed_geos` requests get `InvalidArgument`; Service gets no status IP until configured |
| Invalid code in RPC | `InvalidArgument`; no state mutation |
| Unknown but well-formed code | accepted; empty union set + deny rule = fail closed |
| Provider download fails with cache | old cache and reader stay active; warning logged |
| Provider download fails without cache | daemon exits at startup (fail closed) |
| Corrupt/truncated download | rejected by the MMDB marker check; old cache kept |
| Annotation only invalid entries | treated as absent → allow all (see the sharp edge above) |

The controller treats `InvalidArgument` as non-retryable: the reconcile fails
and logs. There is no per-Service status condition for geo errors yet.

## Testing

Unit coverage:

- `ingress/internal/config`: provider selection, defaults, validation,
  legacy inference.
- `ingress/internal/geoip`: TSV mock, MaxMind and DB-IP download paths, month
  awareness, atomic replace, corrupt-download rejection, reload swap.
- `ingress/internal/geo`: watcher/poller, callback payload, failed reload.
- `ingress/internal/router/nftables`: rule order, union-set create/flush,
  interval encoding, fail-closed empty set.
- `controller/internal/controller`: annotation parsing, `geosChanged`, create
  and update forwarding.
- `controller/internal/router`: proto round-trip for geos.

E2E (`e2e/geo_block_test.go`, mock provider):

| Spec | Asserts |
|---|---|
| rule order and set shape | exactly one geo deny, first; DNAT; final drop; populated `geo_<sn>` |
| allow/deny + annotation flip | US allowed, EU dropped; flipping to EU reverses both without recreating the LB |
| database hot reload | swapping the fixture rows updates the sets without restart |
| per-service isolation | two LBs with different allow-lists do not cross-talk |

The suite uses `10.44.0.0/26=US`, `10.44.0.64/26=CA`, `10.44.0.128/26=EU`,
`10.44.0.192/26=XX`. EU traffic is sourced from a secondary
`10.44.0.129/24` address on `ns-server` (ADR 0005). `BeforeSuite` rewrites the
fixture and resets the dnsmasq lease database, so repeated runs on a kept VM
start from a clean pool.

## Related decisions

- `docs/adr/0001-deny-before-dnat-with-per-service-union-sets.md` — rule shape
- `docs/adr/0002-mock-cidr-carveup-and-ns-external-veth.md` — CIDR carve-up
  (EU topology superseded by ADR 0005)
- `docs/adr/0003-maxmind-optional-and-deferred-validation.md` — optional
  database, RPC validation
- `docs/adr/0004-geo-rebuild-callback-interface.md` — `OnRebuild` /
  `RefreshGeoSets`
- `docs/adr/0005-implementation-seams-and-eu-source.md` — test seams and the
  EU source address
- `docs/adr/0006-dbip-lite-provider.md` — DB-IP support and provider naming

## Known limitations

- Per-Service scope only; different ports of one Service share one allow-list.
- IPv4 only (the nftables table is IPv4).
- No on-demand refresh; timer/file-watch only.
- DB-IP Lite is less accurate than MaxMind (DB-IP's accuracy index: 81 vs 93
  for its commercial database). Country-level allow-lists tolerate this.
- The controller performs format validation only; semantic failures (unknown
  country, no provider) surface in ingress/controller logs rather than a
  Service status condition.
