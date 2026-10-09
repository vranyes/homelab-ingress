# Geoblocking Implementation Plan

Add per-LB-service geoblocking to the homelab-ingress stack. Operators (humans
or K8s controller) attach a list of ISO-3166 alpha-2 country codes to a
service; the ingress daemon enforces them at the kernel level via nftables
set membership, with country data sourced from a MaxMind GeoLite2 Country
database.

## 1. Design Decisions (locked)

- **Geo → CIDR translation lives in the ingress daemon, not the controller.**
  The controller sends only a list of ISO-3166 country codes (e.g. `["US","CA"]`)
  over the existing gRPC API. The ingress daemon owns the MaxMind database and
  is responsible for materialising it as nftables sets.
- **MaxMind DB is fetched by the ingress daemon directly**, not provided via a
  file mount. The daemon holds a MaxMind license key in its YAML config and
  refreshes the database on a timer.
- **K8s side**: Service annotation
  `homelab-ingress.vranyes.com/allowed-geos: "US,CA"`, parsed by the existing
  `controller/internal/controller/annotations.go`. Empty/absent annotation =
  allow all traffic (backward compatible).
- **nftables**: the existing per-LB prerouting chain (`pre_<svc>`) gets a
  geo-accept rule inserted *before* each DNAT rule. The existing final
  `drop` rule at the end of the chain catches everything not explicitly
  accepted (geo-allow-list or DNAT accept).
- **Geo set shape**: a single nftables `Set` keyed by IPv4 address with
  `interval` elements, *per country code*. The set is shared across all
  services that reference the same country code. nftables set lookups are
  kernel-level; this scales to the full ISO-3166 set without user-space
  cost.
- **Set (re)build triggers** (any one is enough to rebuild):
  1. fs-notify watch on the MaxMind DB path.
  2. 30s fallback poller for bind-mounted filesystems where inotify may not
     fire (covers operational edge cases seen in this repo before — see
     `docs/ISSUES.md` P3 #2 for similar dual-trigger patterns).
  3. Daemon startup.

## 2. Affected Components

```
proto/router/v1/router.proto                        # Add allowed_geos to Create/Update/Get
                                                    # Regenerate both controller & ingress stubs

ingress/internal/config/config.go                   # Add MaxMind config block
ingress/internal/state/state.go                     # Add AllowedGeos []string to LBService
ingress/internal/service/service.go                 # Plumb geos between proto and state
ingress/internal/maxmind/ (NEW)                     # MaxMind GeoLite2 Country downloader/reader
ingress/internal/geo/ (NEW)                         # DB watcher + country→nftables set cache
ingress/internal/router/interface.go                # AddRules signature unchanged; Add RefreshGeoSets
ingress/internal/router/nftables/
  ├── const.go                                      # geo_<cc> set name prefix
  ├── helpers.go                                    # validateService: validate ISO codes
  ├── exprs_bldrs.go                                # buildGeoSetAcceptExprs
  ├── build_table.go                                # add geoCIDRSetFor helper
  └── manager.go                                    # AddRules emits geo accept before DNAT
ingress/cmd/ingress/main.go                         # Wire maxmind downloader + geo watcher

controller/internal/controller/annotations.go      # AnnotationAllowedGeos + AllowedGeos()
controller/internal/controller/service_controller.go # Read annotation, send on RPC
controller/internal/router/client.go                # Create/Update signatures take geos
                                                    # ServiceEntry carries AllowedGeos

e2e/harness/ingress.go                              # MaxMindConfig block in IngressConfig
e2e/harness/k8s.go                                 # CreateLBServiceWithGeos helper
e2e/harness/nft.go                                 # AssertGeoSetExists, GetGeoSetCounters
e2e-vm/lima.yaml                                    # Provision a mock GeoLite2 fixture
e2e/geo_block_test.go (NEW)                         # E2E scenarios
```

## 3. Protobuf Changes

Add to `proto/router/v1/router.proto`:

```proto
message CreateLoadBalancerRequest {
  string service_key = 1;
  repeated PortMapping ports = 2;
  repeated string allowed_geos = 3;          // ISO-3166 alpha-2
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

Regenerate Go stubs into both `ingress/pkg/api/router/v1` and
`controller/pkg/api/router/v1` (the latter is vendored today). Field numbers
above are next-available.

Rationale: keep geos at the request level, not per-port. The existing
per-LB nftables chain `pre_<svc>` already gates on `ip daddr <extIP>`, so
geo filtering naturally lives at the same scope.

The ingress daemon's gRPC server validates that `maxmind` is configured
before accepting non-empty `allowed_geos`; returns `InvalidArgument`
otherwise. The controller's existing error handling (non-retryable error
→ log + requeue) surfaces this without special controller-side code.

## 4. Ingress — Implementation

### 4.1 Config (`ingress/internal/config/config.go`)

Add a new `MaxMind` block:

```yaml
maxmind:
  license_key: "<MaxMind license key>"
  edition: "GeoLite2-Country"               # default
  refresh_interval: "24h"                   # default
  cache_path: "/var/lib/homelab-ingress/geolite2-country.mmdb"
```

Validation:
- If `maxmind` block is absent or `maxmind.license_key` is empty: **not an
  error.** Geo-blocking is disabled at the service level: any `allowed_geos`
  annotation on a K8s Service or any `allowed_geos` field in a proto
  `CreateLoadBalancer`/`UpdateLoadBalancer` request returns
  `InvalidArgument` ("MaxMind not configured"). Existing deployments with
  no MaxMind block continue to work unchanged.
- If `cache_path` is empty: default under `--state-dir`.
- If `license_key == "mock"` the daemon uses an internal TSV-backed mock
  reader instead of the MMDB. This is the e2e-only code path.

Add unit tests in `config_test.go` for the new block.

### 4.2 State (`ingress/internal/state/state.go`)

```go
type LBService struct {
    ServiceName   string           `json:"service_name"`
    ...
    Targets       []LBTarget       `json:"targets"`
    AllowedGeos   []string         `json:"allowed_geos,omitempty"`
}
```

Update `Clone()` to copy the slice.

### 4.3 gRPC server (`ingress/internal/service/service.go`)

- `CreateLoadBalancer`, `UpdateLoadBalancer`: copy `req.AllowedGeos` into
  `state.LBService.AllowedGeos`.
- `GetLoadBalancer`, `ListLoadBalancers`: surface `AllowedGeos` back from
  state.

### 4.4 New package `ingress/internal/maxmind/`

Responsibilities:
- Download the GeoLite2-Country `.mmdb` tarball from
  `https://download.maxmind.com/geoip/databases/GeoLite2-Country/update?edition_id=GeoLite2-Country&license_key=<key>`.
- Atomically write to `cache_path` (download to temp file, fsync, `os.Rename`).
- Expose a `Reader` interface:
  ```go
  type Reader interface {
      LookupCountry(net.IP) (string, bool)
      CountryCIDRs(cc string) ([]*net.IPNet, error)
      Close() error
  }
  ```
- Production backend: `github.com/oschwald/maxminddb-golang`.
- Mock backend (when `license_key=="mock"`): a small TSV file with rows
  `<cidr>\t<iso>`. Lets e2e tests inject a deterministic IP→country mapping
  without provisioning a binary MMDB.
- `Run(ctx, refreshInterval)` goroutine: re-downloads on a timer; on first
  run, downloads immediately if `cache_path` does not exist or is older
  than `stale_after` (default 7d).

Add dep: `github.com/oschwald/maxminddb-golang`.

### 4.5 New package `ingress/internal/geo/`

Responsibilities:
- Hold an in-memory map `countryCode → []*net.IPNet`.
- Watch `maxmind.cache_path` via `github.com/fsnotify/fsnotify`. On write
  events, reload.
- Run a 30s fallback ticker that re-reads the file's `mtime` and reloads if
  changed. Belt-and-braces for bind mounts.
- Expose `CountryCIDRs(cc string) ([]*net.IPNet, bool)`.
- Expose `OnRebuild(func(map[string][]*net.IPNet))` — fired after a
  successful reload. geo.Manager builds the `svcCIDRs` map by walking
  `st.Clone().Services`, and for each service with non-empty
  `AllowedGeos`, concatenates the CIDRs for all its allowed countries
  from the freshly-reloaded cache. The callback (set by main.go) is
  typically `iml.RefreshGeoSets`.
- Expose `SnapshotInUse() map[string]struct{}` derived from `state.State`
  (only geos referenced by at least one active service need to be
  materialised in the kernel).

Add dep: `github.com/fsnotify/fsnotify`.

### 4.6 nftables (`ingress/internal/router/nftables/`)

**const.go**

```go
const geoSetPrefix = "geo_"  // appended with safeName(serviceKey) for per-service sets
```

**helpers.go** — extend `validateService`:

```go
var isoAlpha2RE = regexp.MustCompile(`^[A-Z]{2}$`)
// in validateService:
for _, cc := range svc.AllowedGeos {
    if !isoAlpha2RE.MatchString(cc) {
        return fmt.Errorf("invalid ISO-3166 alpha-2 code %q", cc)
    }
}
```

**exprs_bldrs.go** — add

```go
// buildGeoSetDenyExprs returns: ip daddr <extIP> ip saddr not in @<geoSetName> counter drop
func buildGeoSetDenyExprs(extIP net.IP, geoSetName string) []expr.Any { ... }
```

The rule uses a single `ip saddr @geoSet` lookup with `not in` semantics
(one radix-tree search per packet). No per-country Lookup chain.

**build_table.go** — add `buildGeoUnionSet(c, table, sn, cidrs) (*nftables.Set, error)`:

- Set name: `geo_<sn>` (where `sn = safeName(serviceKey)`).
- If a set named `geo_<sn>` already exists in `table`: `FlushSet`, then
  `SetAddElements` with the union CIDRs as `nftables.SetElement` with
  `IntervalEnd` for ranges.
- Otherwise: create with `KeyType: nftables.TypeIPAddr`, key bytes 4,
  interval semantics.
- GeoLite2-Country CIDRs are globally disjoint across countries, so the
  union is a concatenation with no overlaps — the kernel interval set
  rejects duplicate ranges (`-EEXIST`), but disjoint data avoids this.
- Returns the populated set, ready for `Lookup(..., Invert: true)`.
- The existing `RemoveRules` cleanup (manager.go:262–270) deletes any
  set whose name contains the service key — `geo_<sn>` is cleaned up
  automatically when the LB is removed, so no set-GC subsystem is needed.

**manager.go `AddRules`** — revised flow:

1. Validate service (existing).
2. Find or create `pre_<svc>` chain (existing).
3. If service exists: `FlushChain` (existing).
4. **NEW**: if `len(svc.AllowedGeos) > 0`:
   - Resolve CIDRs for **all** allowed countries via the injected
     `geo.Lookup` interface (`nftables.WithGeoLookup(geoMgr)` option).
   - Concatenate the per-country CIDR slices into a flat `[]*net.IPNet`.
   - Call `buildGeoUnionSet` to create/refresh `geo_<sn>`.
   - Append a single deny rule:
     `ip daddr <extIP> ip saddr not in @geo_<sn> counter drop`.
5. Existing DNAT rules per `PortMapping` (unchanged).
6. Existing final `drop` rule (unchanged).

**manager.go new method**:

```go
// RefreshGeoSets rebuilds per-service geo sets after a MaxMind DB reload.
// It walks all active services and calls AddRules so geo union sets are
// rebuilt with fresh CIDRs. nftables set elements are updated in-place;
// no chain flush is needed because set references (SetID) remain valid
// through FlushSet + SetAddElements.
func (m *Manager) RefreshGeoSets(svcCIDRs map[string][]*net.IPNet) error { ... }
```

Where `svcCIDRs` is keyed by service key (the `state.LBService.ServiceName`
format `namespace/name`), values are the concatenated CIDRs for all
allowed countries of that service.

`RefreshGeoSets` re-uses the same nftables connection and touches only
set elements — it does NOT flush the per-service chain (the deny rule
pointing at `geo_<sn>` remains valid as long as the set object itself
is not deleted).

### 4.7 Wiring (`ingress/cmd/ingress/main.go`)

The nftables Manager receives two callbacks from the geo layer:

- **`WithGeoLookup(geoMgr.CountryCIDRs)`** — provides a `func(cc string) ([]*net.IPNet, bool)` used at `AddRules` time to resolve country codes to CIDRs for a single service. The nftables Manager concatenates results for all allowed countries and creates the per-service union set.
- **`RefreshGeoSets`** — called on MaxMind DB reload via the geo layer's rebuild callback. It re-resolves CIDRs for all active services and flushes+repopulates each per-service union set.

```go
mm := maxmind.New(cfg.MaxMind, logger)
geoMgr := geo.NewManager(mm, st, logger)
go mm.Run(ctx, cfg.MaxMind.RefreshInterval)

iml := nftables.NewManager(logger,
    append(nftOpts, nftables.WithGeoLookup(geoMgr.CountryCIDRs))...)

// DB reload → geo.Manager triggers a full RefreshGeoSets pass
geoMgr.OnRebuild(func(svcCIDRs map[string][]*net.IPNet) {
    if err := iml.RefreshGeoSets(svcCIDRs); err != nil {
        logger.Error("failed to refresh geo sets", "error", err)
    }
})
go geoMgr.Run(ctx)
```

The `geo.Manager` computes `svcCIDRs` by calling `SnapshotInUse()` (derived from `st.Clone()`) and resolving each referenced country via its cached CIDR map.

### 4.8 Ingress unit tests

- `ingress/internal/maxmind/maxmind_test.go`:
  - TSV-backed mock reader: `LookupCountry`, `CountryCIDRs`.
  - Mocked HTTP download server returns a canned tarball; verify atomic
    file replacement; verify the previous reader is still usable until the
    new one is loaded.
- `ingress/internal/geo/manager_test.go`:
  - Watcher fires on file swap.
  - Fallback 30s poller fires when fsnotify doesn't deliver events
    (simulated by writing through `os.Rename`).
- `ingress/internal/router/nftables/nftables_test.go`:
  - Extend with `TestAddRules_GeoAllowList`: an `LBService` with
    `AllowedGeos: ["US","CA"]` produces
    - one `add set geo_<sn>` batched message with the union of US + CA CIDRs,
    - one `add rule ip daddr <extIP> ip saddr not in @geo_<sn> drop`
      rule emitted *before* the DNAT rule expressions,
    - the existing DNAT rules and final `drop` rule unchanged.
  - Add `TestAddRules_GeoEmpty`: an `LBService` with `AllowedGeos: nil/empty`
    produces no geo set and no deny rule (backward compat).

## 5. Controller — Implementation

### 5.1 Annotations (`controller/internal/controller/annotations.go`)

```go
const AnnotationAllowedGeos = AnnotationPrefix + "/allowed-geos"

func AllowedGeos(svc *corev1.Service) []string {
    if svc.Annotations == nil { return nil }
    raw := svc.Annotations[AnnotationAllowedGeos]
    if raw == "" { return nil }
    out := make([]string, 0, 2)
    for _, cc := range strings.Split(raw, ",") {
        cc = strings.ToUpper(strings.TrimSpace(cc))
        if len(cc) == 2 {
            out = append(out, cc)
        }
    }
    return out
}
```

Add `annotations_test.go` (currently missing — this also closes the test
gap flagged in `docs/ISSUES.md` §5).

### 5.2 Service controller (`controller/internal/controller/service_controller.go`)

In `reconcileUpsert`, after computing `ports`:

```go
geos := AllowedGeos(svc)
```

Pass to `r.RouterClient.Create` / `Update`. Extend both signatures to take
`geos []string`. Mirror the existing `portsChanged` comparator with a
`geosChanged` comparator and trigger `Update` when either changes.

If the ingress daemon returns `InvalidArgument` (MaxMind not configured),
the controller treats it as a non-retryable error per the existing error
handling (`router.IsRetryable` returns false for `InvalidArgument`) —
the reconcile fails, surfaces in controller logs, and the next reconcile
event re-triggers once MaxMind is configured. No special error handling
code needed for this path.

`reconcileUpsert` returns the same `RequeueAfter` cadence regardless —
geo churn is rare and the existing 5s requeue absorbs it.

### 5.3 Controller router client (`controller/internal/router/client.go`)

```go
type ServiceEntry struct {
    ServiceKey   string
    AssignedIP   string
    Ports        []PortMapping
    AllowedGeos  []string         // NEW
    DHCPAcquired bool
    Interface    string
}

type Client interface {
    Create(ctx context.Context, serviceKey string, ports []PortMapping, geos []string) (*ServiceEntry, error)
    Update(ctx context.Context, serviceKey string, ports []PortMapping, geos []string) error
    ...
}
```

Update `toProtoPorts`/the proto request builders to forward `geos` directly.
Update `fromProto` to populate `AllowedGeos` from `resp.AllowedGeos`.

Update the existing `client_test.go` `fakeServer` to record and echo
geos. Add test cases for:

- `TestClient_Create_ForwardsGeos` — verify the fakeServer sees the right
  country codes on `CreateLoadBalancer`.
- `TestClient_Update_ForwardsGeos` — same for `Update`.
- `TestClient_Get_ReturnsGeos` — verify `ServiceEntry.AllowedGeos` is
  populated from `GetLoadBalancerResponse.AllowedGeos`.

### 5.4 Deploy YAML (`controller/deploy/deployment.yaml`)

No changes — the controller needs no new env var or volume.

## 6. E2E Testing Plan

Both halves of the question:

1. **Ingress-side functionality**: covered by unit tests in `ingress/internal/...`
   (§4.8). The nftables layer is the contract; those tests prove the
   contract directly via the existing `nftables.WithTestDial` mock.

2. **E2E functionality (ingress + controller)**: covered by Ginkgo specs in
   `e2e/geo_block_test.go`. The existing e2e suite already provides the
   harness for: a fake OIDC issuer, the ingress daemon spawned with a
   config, the k3s controller, a `homelab-ingress` LB class, and a
   bridge-attached network for hitting LB IPs. We extend it for geo.

### 6.1 Mock MaxMind fixture

In `e2e-vm/lima.yaml` (provision script `scripts/03-geolite-mock.sh`):

- Write `/var/lib/homelab-ingress/mock-geo.tsv`:
  ```
  10.44.0.0/26    US
  10.44.0.64/26   CA
  10.44.0.128/26  EU
  10.44.0.192/26  XX
  ```
  This is read by `ingress/internal/maxmind` when `license_key=="mock"`.
  The `hl-br0` bridge itself (10.44.0.1/24) falls in the `US` quarter, so
  the test process sourcing from 10.44.0.1 appears as `US`.
- Carve the test bridge subnet (`10.44.0.0/24`) into four /26 ranges.
  Each /26 maps to a distinct ISO-3166 alpha-2 code:
  - `10.44.0.0/26  US` — bridge, test process, any host on hl-br0.
  - `10.44.0.64/26 CA` — reserved for future multi-country tests.
  - `10.44.0.128/26 EU` — assigned to the ns-external veth endpoint.
  - `10.44.0.192/26 XX` — reserved.
- Rationale for replacing `127.0.0.0/8` with `10.44.0.128/26`:
  the Linux kernel drops packets sourced from the loopback range
  (`127.0.0.0/8`) on non-lo interfaces at the IP layer. A fake EU
  CIDR carved from the existing test bridge subnet works cleanly:
  the kernel has no objection to routing `10.44.x.x` across interfaces.

### 6.2 Harness extensions

- `e2e/harness/ingress.go` — extend `DefaultIngressConfig` and
  `IngressConfig` struct with a `MaxMind` block using the mock license key.
- `e2e/harness/k8s.go` — add
  ```go
  func CreateLBServiceWithGeos(ctx context.Context, cl client.Client, ns, name string,
      externalPort, targetPort int32, geos []string) error
  ```
  which sets the `homelab-ingress.vranyes.com/allowed-geos` annotation.
- `e2e/harness/nft.go` — add
  - `AssertGeoSetExists(cc string)` — opens an nftables conn, walks chains
    in the `homelab_ingress` table, returns nil if a set named `geo_<cc>`
    exists (use `conn.GetSets`).
  - `GetRuleCounters(tableName, chainName)` is already exported and is
    what we use to count geo-accept and drop rule hits.

### 6.3 Ginkgo specs (`e2e/geo_block_test.go`)

Four specs. Each models existing patterns from `one_lb_test.go` and
`two_lb_masq_test.go`.

**Spec A — "geo allow-list applied — denied traffic dropped at prerouting"**

- Deploy 1 backend `geo-allow-lb`.
- Create LB Service with annotation `allowed-geos: "US"`.
- Wait for status IP `lbIP`.
- Wait for nftables `geo_US` set and `pre_<svc>` chain to both exist
  (`AssertGeoSetExists("US")`, `AssertLBChainExists(svcKey)`).
- Snapshot rule counters on the chain via `GetRuleCounters`.
- Fire 20 HTTP GETs to `lbIP:80`. Source IP is `10.44.0.1` (US range).
- Assertions: only the geo-accept counter increases; the drop-rule counter
  is zero.

**Spec B — "geo denial — non-allow-listed country is dropped before DNAT"**

- Same LB as Spec A.
- Create a netns `ns-external` and a veth pair to connect it to `hl-br0`:
  ```go
  RunShell(ctx,
      "ip netns add ns-external && " +
      "ip link add veth-ext-us type veth peer name veth-ext-eu && " +
      "ip link set veth-ext-us master hl-br0 && " +
      "ip link set veth-ext-us up && " +
      "ip link set veth-ext-eu netns ns-external && " +
      "ip -n ns-external addr add 10.44.0.129/24 dev veth-ext-eu && " +
      "ip -n ns-external link set veth-ext-eu up")
  ```
  `DeferCleanup` removes the netns (deletes veth-ext-us automatically).
  - `veth-ext-us` (root ns) enslaved to `hl-br0` — gives the namespace
    L2 connectivity to the entire bridge.
  - `veth-ext-eu` (inside ns-external) with `10.44.0.129/24` — source
    address falls in the `EU` mock CIDR (`10.44.0.128/26`). The `/24`
    netmask gives the namespace a connected route for the full bridge
    subnet, so LB IPs in the `10.44.0.10–10.44.0.100` range are
    reachable.
- Snapshot counters on the chain.
- From inside `ns-external` (`ip netns exec ns-external curl ...`), send 10
  HTTP GETs to `lbIP:80`. Source IP `10.44.0.129` falls in the `EU` mock
  CIDR. The packet egresses via `veth-ext-eu` → veth pair → `veth-ext-us`
  → `hl-br0` → arrives at the macvlan child as an *incoming* packet,
  traversing the prerouting nat chain.
- Assertions: drop-rule (geo-deny) packet counter delta == 10 (every request
  was dropped). The DNAT rule counter delta == 0 (no DNAT ever fired). This
  proves the geo check runs at prerouting *before* DNAT, which is the
  security-critical property.

**Spec C — "geo set hot-reload on MaxMind DB swap"**

- Start with the mock fixture mapping `10.44.0.0/24=US`.
- Deploy `geo-reload-lb` with `allowed-geos: "US"`. Verify Spec A
  succeeds (traffic flows from `10.44.0.1`).
- Atomically overwrite the fixture so `10.44.0.0/24=EU` (and US is now
  empty / mapped to a different range). Use `harness.WriteFile` for the
  atomic rename.
- Within ~30s (fsnotify + fallback poller), the geo set should rebuild.
  Assert: drop-rule packet counter delta > 0 for new GETs from
  `10.44.0.1`.
- This validates that the controller does not need to be involved in
  MaxMind refresh — the change is fully owned by the ingress daemon.

**Spec D — "geo isolation across two LBs with different allow lists"**

- Two LBs: `lb1` with `allowed-geos: "US"`, `lb2` with `allowed-geos: "EU"`.
- Distinct backend IDs `b1`, `b2` (existing fixture).
- From `10.44.0.1` (US): `lb1` reaches `b1`, `lb2` drops. Verify via
  drop-rule counter deltas + backend IDs in the response.
- From `ns-external` (EU, `10.44.0.129`): `lb2` reaches `b2`, `lb1` drops.
- Asserts per-service geo isolation via drop/DNAT counter deltas on each
  chain. The geo sets are per-service (`geo_<sn>`), not per-country, but
  the isolation property (two services enforce different geo scopes
  independently) is what the test validates.

### 6.4 Test matrix summary

| Spec | Ingress side tested                                     | Controller side tested                          | Validates                                         |
|------|---------------------------------------------------------|-------------------------------------------------|---------------------------------------------------|
| A    | `nftables.AddRules` emits geo-deny rule + per-service union set; geo.Manager caches `US` CIDRs | Annotation parsed → sent on RPC → applied        | happy path                                        |
| B    | geo-deny rule at prerouting drops non-allowed source before DNAT | Annotation parsing round trip                   | security property: drop precedes DNAT             |
| C    | fsnotify hot-reload rebuilds geo sets without restart   | (controller not involved)                       | MaxMind refresh path is local to ingress          |
| D    | Two LBs enforce different geo allow-lists independently | Distinct annotation per Service                 | per-service geo isolation                         |

## 7. Phased Implementation

Each phase is shippable in isolation; later phases depend on earlier ones.

**Phase 1 — Proto surface prep** (single commit, no behavior change)

- Add `allowed_geos` to `router.proto`.
- Regenerate both `ingress/pkg/api/router/v1` and
  `controller/pkg/api/router/v1` stubs.
- Plumb `AllowedGeos` through `state.LBService`, the gRPC server, the
  controller router client, and `fakeServer` in `client_test.go`.
- Add unit tests for proto round-trip on both sides.
- All existing tests stay green; backward compatibility is preserved
  because empty `allowed_geos` is a no-op everywhere.

**Phase 2 — Ingress: state + nftables rules**

- Add `MaxMind` config block + validation + unit tests.
- Add `internal/maxmind/` (downloader + Reader + mock backend).
- Add `internal/geo/` (watcher + per-country CIDR cache).
- Extend `nftables/helpers.go`, `exprs_bldrs.go`, `build_table.go`,
  `manager.go` per §4.6.
- Wire in `cmd/ingress/main.go`.

**Phase 3 — Controller annotation + RPC**

- Add `AnnotationAllowedGeos` + `AllowedGeos()` + `annotations_test.go`.
- Extend `service_controller.go reconcileUpsert` per §5.2.
- Update `controller/internal/router/client.go` signatures and tests
  (§5.3).

**Phase 4 — E2E harness + mock fixture**

- Add `e2e-vm/scripts/03-geolite-mock.sh` and wire it into `lima.yaml`.
- Extend `harness/ingress.go`, `harness/k8s.go`, `harness/nft.go` per §6.2.

**Phase 5 — E2E specs**

- Implement Specs A–D from §6.3 in `e2e/geo_block_test.go`.
- Run `make test-e2e`. Pre-existing specs must remain green.

## 8. Risks & Mitigations

| Risk                                                              | Mitigation                                                                                                              |
|-------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------|
| MaxMind MMDB lookup at packet time would be catastrophic         | Geo lookup happens at config time; nftables does set membership in the kernel — not user space.                          |
| Stale MaxMind data because nobody triggers a refresh             | fsnotify hot reload + 30s fallback poller; Spec C catches drift.                                                         |
| Per-service chain grows unbounded                                 | Each `pre_<svc>` gets one `ip saddr @geo_<cc> accept` per allowed country — bounded by ISO-3166 (≤249).                  |
| Controller sends geo codes the daemon doesn't recognise          | Daemon validates ISO-2 format (regex) and either warns + ignores unknown codes or rejects with `InvalidArgument`.        |
| Integration of fsnotify + nftables rebuilds adds concurrency      | Single goroutine owns geo-set rebuild; dispatches via `router.Manager.RefreshGeoSets`. Reuses existing `Manager.mu`.      |
| Mock MMDB provisioning in lima.yaml could rot                     | Mock backend is plain TSV, not a binary MMDB. `maxmind.New` switches to the mock reader when `license_key=="mock"`.      |
| Spec B's `ns-external` namespace races other tests                | Created and torn down inside the spec via `DeferCleanup`, mirroring `EnsureVepaRelayUp`.                                 |
| Annotation key collisions with future fields                      | Reuse the existing `AnnotationPrefix` constant; new constants are siblings of `AnnotationSkip`.                          |
| Stub `nftables_stub.go` (non-linux build) hides bugs              | Phase 2 keeps the stub calling `validateService` (already does); geo validation goes through `validateService` too.      |

## 9. Open Questions

1. Annotation key — confirm
   `homelab-ingress.vranyes.com/allowed-geos` matches the existing
   `AnnotationPrefix` (`controller/internal/controller/annotations.go:8`).
2. ISO code format — alpha-2 ("US","CA") vs alpha-3 ("USA","CAN"). Plan
   assumes alpha-2 because MaxMind GeoLite2-Country uses alpha-2. alpha-3
   would force a translation layer in the controller.
3. Mock strategy — TSV-backed reader (simple, vendored testdata only) vs
   vendoring a small real MMDB (~30KB) read with the real
   `maxminddb-golang`. Plan uses TSV for simplicity.
4. Geo scope — per-LB (one set per LB, one rule per country) vs per-port
   (`PortMapping.AllowedGeos` allowing e.g. port 80 only US, port 443
   US+CA). Plan assumes per-LB only. Per-port would move the proto field
   inside `PortMapping` and add geo accept rules *after* the protocol/port
   matcher in the prerouting chain.
