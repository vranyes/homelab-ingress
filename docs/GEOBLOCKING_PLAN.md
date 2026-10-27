# Geoblocking: design and as-built notes

Per-LoadBalancer country filtering for homelab-ingress. Operators attach a list
of ISO-3166 alpha-2 country codes to a Service; the ingress daemon resolves
them to IPv4 CIDRs and enforces the list in nftables before DNAT.

As-built as of ADR 0007; original plan in git history.

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
Service annotations
  homelab-ingress.vranyes.com/allowed-geos: "US,CA"
  homelab-ingress.vranyes.com/allowed-cidrs: "192.168.1.0/24"
            │
            ▼
controller: AllowedGeos() + AllowedCIDRs() → Create/Update RPC (allowed_geos, allowed_cidrs)
            │
            ▼
ingress gRPC: normalize + validate ISO-3166 alpha-2 codes and IPv4 CIDRs
            │
            ▼
state.LBService.AllowedGeos + AllowedCIDRs (persisted in the `services` table of `state.db`)
            │
            ▼
nftables AddRules: resolve countries + parse static CIDRs → per-service union set geo_<sn>
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
metadata:
  name: web # + type: LoadBalancer, loadBalancerClass: homelab-ingress
  annotations:
    homelab-ingress.vranyes.com/allowed-geos: "US,CA"
```

Remove the annotation to allow all traffic:

```bash
kubectl annotate svc web homelab-ingress.vranyes.com/allowed-geos-
```

A Service is allow-all only when **both** annotations are absent/empty.

Parser semantics (`controller/internal/controller/annotations.go`):

- `allowed-geos`: comma-separated codes, trimmed, uppercased, de-duplicated
  (`" us, ca,US "` → `["US","CA"]`); invalid entries are dropped; absent,
  empty, or all-invalid → `nil` (allow all, unless `allowed-cidrs` is set).
- `allowed-cidrs`: comma-separated IPv4 CIDRs, trimmed, canonicalised via
  `IPNet.String()`, de-duplicated (`" 192.168.1.1/24 "` → `["192.168.1.0/24"]`);
  empty entries are skipped; **any** structurally invalid entry (unparseable
  CIDR, IPv6) returns an error and the controller logs it and keeps the
  last-good CIDRs (`nil` on first install) while still reconciling
  (fail closed, no hot-loop). Absent or empty → `nil`.

### Static CIDR bypass (`allowed-cidrs`)

Private IPs (RFC 1918, CGNAT, link-local) have no country in MaxMind/DB-IP, so
an `allowed-geos` Service drops LAN traffic. The `allowed-cidrs` annotation
(`homelab-ingress.vranyes.com/allowed-cidrs: "192.168.1.0/24, 10.0.0.0/8"`)
adds static IPv4 CIDRs that are always allowed alongside the country list:

- Either annotation non-empty → deny-by-default with a union set (country
  CIDRs ∪ static CIDRs). Both empty → allow all (backward compatible).
- No magic country codes: the union is explicit in the annotation and in
  `state.LBService` (`allowed_geos` + `allowed_cidrs` JSON fields).
- Static CIDRs are enforced even with `geoip.provider` unset; only
  `allowed-geos` gates on the database (ADR 0003). The nftables deny rule is
  emitted for CIDR-only Services with no geo lookup configured.
- Scope is IPv4 only (the nftables table ignores IPv6); IPv6 CIDRs are
  rejected at every layer (controller annotation, gRPC validation, nftables
  `validateService`).
- `geo.Manager.ServiceCIDRs()` unions `CountryCIDRs` with the parsed static
  CIDRs (invalid entries skipped with a log — defence in depth, the RPC layer
  already rejected them). `SnapshotInUse` stays country-only (cache
  bookkeeping).

> **Operational sharp edge:** typo'd geos (e.g. `USA`) are dropped and
> normalise to nil, so the Service fails OPEN (unrestricted). CIDR typos
> fail CLOSED: the controller keeps the last-good CIDRs and still
> reconciles. Review both annotations as security config.

## gRPC interface

`proto/router/v1/router.proto`:

```proto
message CreateLoadBalancerRequest {
  string service_key = 1;
  repeated PortMapping ports = 2;
  repeated string allowed_geos = 3; // ISO-3166 alpha-2
  // ... plus rate_limit (see ADR 0007), omitted here
  repeated string allowed_cidrs = 5; // IPv4 CIDRs always allowed (e.g. LAN ranges)
}
// Update carries the same allowed_geos/allowed_cidrs fields; Get carries
// them as fields 6 and 8.
```

`Get`/`List` populate `ServiceEntry.AllowedGeos` and `AllowedCIDRs`, which
lets the controller compare desired state (annotations) with the router's
actual state. Both are compared as sets, so reordering an annotation does
not churn the router.

## Providers and configuration

```yaml
geoip:
  provider: dbip            # maxmind | dbip | mock
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

- `cache_path` defaults to `<state-dir>/<provider-file>`; OpenWrt overrides,
  see `docs/openwrt.md`.
- `stale_after`: 7d (maxmind/mock), 35d (dbip). Bare `license_key` implies
  `maxmind`; `"mock"` selects mock.

DB-IP Lite is free but licensed CC BY 4.0. The daemon logs
`geo data by DB-IP (https://db-ip.com)` at startup when selected; keep that
attribution in operator-facing material.

Downloads are atomic (temp + fsync + verify + rename); failures keep the old
cache.

## nftables enforcement

For a service with a non-empty allow-list (`allowed-geos` or `allowed-cidrs`),
the per-service chain is:

```text
1. ip daddr <extIP> ip saddr not in @geo_<sn> counter drop
2. (if rate-limited) per-client new-connection limit rule (see ADR 0007)
3. one DNAT rule per target (protocol + port)
4. ip daddr <extIP> counter drop
```

The deny rule **must** come first: an `accept` verdict in nat prerouting
terminates chain evaluation and would skip DNAT (ADR 0001).

- Set name: `geo_<sn>`, where `sn = safeName(serviceKey)`.
- One interval set per service, containing the union of all allowed countries'
  IPv4 CIDRs **plus** the static `allowed-cidrs` CIDRs.
- Existing sets are flushed and repopulated in place; the deny rule's `SetID`
  stays valid, so no chain rebuild is needed on a database reload (ADR 0004).
- The existing `RemoveRules` cleanup deletes sets whose name contains `sn`.
- An unknown country (valid alpha-2, no CIDRs in the database) logs a warning,
  contributes no CIDRs, and keeps the deny rule — the service **fails closed**.
- An invalid static CIDR in `validateService` rejects the whole service update;
  at set-build time a bad entry is skipped with a warning so the rest of the
  union still installs.
- Services with neither `allowed-geos` nor `allowed-cidrs` keep the original
  DNAT-then-drop shape.

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

## Failure semantics

| Situation | Behaviour |
|---|---|
| No `geoip.provider` | daemon runs; `allowed_geos` requests get `InvalidArgument` (Service gets no status IP until configured); `allowed_cidrs`-only requests are accepted |
| Invalid code in RPC | `InvalidArgument`; no state mutation |
| Invalid CIDR in RPC | `InvalidArgument`; no state mutation |
| Invalid CIDR in annotation | controller logs, keeps last-good CIDRs (`nil` on create), still reconciles ports/geos/status |
| Provider download fails without cache | daemon exits at startup (fail closed) |

The controller treats `InvalidArgument` as non-retryable: the reconcile fails
and logs. There is no per-Service status condition for geo errors yet.

## Testing

Unit: config (selection, defaults, validation), geoip (mock/MaxMind/DB-IP
paths, atomic replace), geo (watcher, rebuild callback), nftables (rule
order, set flush, interval encoding, fail-closed), controller (annotation
parsing, forwarding), proto round-trip.

E2E (`e2e/geo_block_test.go`, mock provider):

| Spec | Asserts |
|---|---|
| rule order and set shape | exactly one geo deny, first; DNAT; final drop; populated `geo_<sn>` |
| allow/deny + annotation flip | US allowed, EU dropped; flipping to EU reverses both without recreating the LB |
| database hot reload | swapping the fixture rows updates the sets without restart |
| per-service isolation | two LBs with different allow-lists do not cross-talk |

Fixture carves `10.44.0.0/24` into `/26`s for US/CA/EU/XX; EU traffic uses a
secondary `10.44.0.129/24` on `ns-server` (ADR 0005).

## Related decisions

- `docs/adr/0001-deny-before-dnat-with-per-service-union-sets.md`
- `docs/adr/0002-mock-cidr-carveup-and-ns-external-veth.md`
- `docs/adr/0003-maxmind-optional-and-deferred-validation.md`
- `docs/adr/0004-geo-rebuild-callback-interface.md`
- `docs/adr/0005-implementation-seams-and-eu-source.md`
- `docs/adr/0006-dbip-lite-provider.md`

## Known limitations

- Per-Service scope only; different ports of one Service share one allow-list.
- IPv4 only (the nftables table is IPv4).
- No on-demand refresh; timer/file-watch only.
- DB-IP Lite is less accurate than MaxMind; country-level allow-lists tolerate this.
- The controller performs format validation only; semantic failures (unknown
  country, no provider) surface in ingress/controller logs rather than a
  Service status condition.
