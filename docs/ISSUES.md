# Ingress Known Issues

Documented during code reviews of the `ingress` and `controller` packages.
Severity: **P0** (critical — system doesn't work as designed), **P1** (major), **P2**
(design weakness), **P3** (minor).

Update this document when fixing an issue.

---

## P0 — Critical

*All critical issues have been resolved.*

## P1 — Major

*All major issues have been resolved.*

## P2 — Design Weaknesses

### P2-1. `SetIP` removes all non-matching addresses (single-address contract)

- **File:** `ingress/internal/macvlan/manager.go` (`SetIP`)
- **Design:** Iterates all IPv4 addresses on the interface and removes **any** that
  don't exactly match `ip + subnetMask`.
  The macvlan interface is fully managed: contract is a single address per
  interface, and foreign addresses are removed during reconcile.
- **Introduced:** Original implementation.
- **Status:** By design — not a bug.

## P3 — Minor

### P3-1. Uncoordinated locks between `State` and `Store`

- **File:** `ingress/internal/state/state.go` and `ingress/internal/state/store.go`
- **Root cause:** `State` uses `sync.RWMutex`, `Store` uses `sync.Mutex`.
  `UpdateService` releases the State lock before calling store methods, so a reader
  can see the new state while the previous version is still on disk.
- **Impact:** On crash between the state update and persist completion, the latest
  write is lost. Acceptable trade-off for performance: the system recovers from
  persisted state on restart, and unpersisted services are reconciled within 30s
  (macvlan/DHCP; router: 5 minutes).
- **Introduced:** Original implementation.
- **Status:** Documented trade-off — no fix planned.

### P3-2. Interface name collision risk (SHA1 truncated to 32 bits)

- **File:** `ingress/internal/macvlan/mac.go:23-33`
- **Root cause:** `InterfaceName` uses SHA1 truncated to 8 hex characters (32 bits,
  ~4 billion values). Collisions are theoretically possible with hundreds of
  thousands of services.
- **Impact:** A collision silently overwrites an existing interface's state.
  Both services would share the same macvlan interface.
- **Mitigation:** Negligible below ~1000 services — no fix.
- **Introduced:** Original implementation.
- **Status:** Mitigated — sufficient for target scale.

## Test coverage (previously gap items, now covered)

All of the following packages have unit tests (run `make test-unit`; the
`//go:build linux` packages also run in-VM via `make test-unit-vm`):

| Package | Tests |
|---|---|
| `ingress/internal/auth` | `auth_test.go` |
| `ingress/internal/dhcp` | `manager_test.go`, `lease_test.go` |
| `ingress/internal/service` | `service_test.go`, `ratelimit_test.go` |
| `ingress/internal/state` | `state_test.go`, `store_test.go`, `ratelimit_test.go` |
| `ingress/internal/router/nftables` | `nftables_test.go`, `geo_test.go`, `ratelimit_test.go`, `validate_test.go` |
| `ingress/internal/macvlan` | `mac_test.go` |
| `ingress/internal/config` | `config_test.go` |
| `ingress/internal/geo`, `geoip`, `certgen` | `manager_test.go` / `certgen_test.go` |
| `controller/internal/controller`, `controller/internal/router` | `controller_test.go`, `ratelimit_test.go`, `client_test.go` |
| `e2e/harness` | `process_test.go`, `traffic_test.go` |

Known gap: `ingress/internal/ws` has no unit tests.

## Code Quality Observations

### P3-3. `endpointInfoForService` is complex

- **File:** `controller/internal/controller/service_controller.go:287`
- **Impact:** Function handles EndpointSlice enumeration, named port resolution,
  and Pod fallback logic. Complex branching makes it hard to test all paths.
- **Fix:** Consider extracting:
  - `collectEndpointIPs()` — IP collection from EndpointSlices
  - `collectNamedPorts()` — port name → number mapping
  - `fallbackToPods()` — Pod-based IP collection
- **Introduced:** Original implementation.
- **Status:** Refactoring recommended but not critical.

### P3-4. `realClient` struct has 13 fields

- **File:** `ingress/internal/dhcp/real.go:18-32`
- **Impact:** 13 fields, all used — connection state, lease state, and lifecycle
  coordination in one client. Acceptable, no action.

## Resolved Issues (Reference)

The following issues were identified in earlier reviews and have been fully resolved:

| Issue | Severity | Description | Resolution |
|-------|----------|-------------|------------|
| 1 | P0 | Shared unbuffered channel | `State.Subscribe()` / `notify()` broadcast pattern |
| 2 | P0 | `GetLoadBalancer` nil-pointer | Nil checks on `DHCPAcquired` and `Interface` |
| 3 | P0 | CLI port parsing bug | Multiple `Destination` entries per port mapping |
| 4 | P1 | No macvlan reconciliation | 30s reconcile loop in `NetlinkManager.Run()` |
| 5 | P1 | Optimistic state write | `break` on `Create()` error before state mutation |
| 6 | P2 | Create/Delete name inconsistency | Both use `InterfaceName(serviceKey)` |
| 7 | P2 | `Health` reports stale count | `ActiveMacvlanFunc` callback to `NetlinkManager.ActiveCount()` |
| 8 | P3 | Hash collision risk | Hash suffix increased to 8 hex chars |
| 9 | P3 | No macvlan tests | `mac_test.go` with 9 tests |
| 10 | P3 | README `LinkManager` fiction | Updated to document `NetlinkManager` methods |
| 11 | P3 | `endpointIPsForService` dead code | Deleted (superseded by `endpointInfoForService`) |
| 12 | P3 | Missing unit tests (auth, dhcp, service, state, nftables) | Test files added for all listed packages |
| 13 | P3 | `rulesNeedUpdate` complexity | Function removed during nftables rewrite |

## Summary by Severity

| Severity | Open | Resolved | Status |
|----------|------|----------|--------|
| P0       | 0    | 3        | All critical issues fixed |
| P1       | 0    | 2        | All major issues fixed |
| P2       | 1    | 2        | One by-design behavior documented |
| P3       | 4    | 6        | 2 trade-offs documented, 1 complexity concern, 1 struct observation |

**Overall System Health:** The ingress system is production-ready for homelab deployments.
All critical and major issues have been resolved. One test gap remains
(`ingress/internal/ws` has no unit tests).

**Recommended Next Actions:**
1. Add unit tests for `ws` package (the last untested package)
2. Consider refactoring `endpointInfoForService` during next maintenance window
