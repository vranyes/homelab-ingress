# Ingress Known Issues

Documented during a code review of the `ingress` package.
Severity: **P0** (critical — system doesn't work as designed), **P1** (major), **P2**
(design weakness), **P3** (minor).

---

Update this document when fixing an issue.

---

## P0 — Critical

*All critical issues have been resolved.*

---

## P1 — Major

*All major issues have been resolved.*

---

## P2 — Design Weaknesses

### 1. `SetIP` removes all non-matching addresses (single-address contract)

- **File:** `ingress/internal/macvlan/manager.go:135-180`
- **Design:** Iterates all IPv4 addresses on the interface and removes **any** that
  don't exactly match `ip + subnetMask`.
  This is intentional — the macvlan interface is a fully managed resource where
  only the ingress service's address should exist.
- **Contract:** Single address per interface.
  Any address not defined by the service owning this macvlan interface is removed
  during reconcile.
- **Introduced:** Original implementation.
- **Status:** By design — not a bug.

---

## P3 — Minor

### 2. Uncoordinated locks between `State` and `Store`

- **File:** `ingress/internal/state/state.go` and `ingress/internal/state/store.go`
- **Root cause:** `State` uses `sync.RWMutex` (`state.go:12`), `Store` uses `sync.Mutex`
  (`store.go:16`).
  `UpdateService` releases the State lock before calling store methods, so a reader
  can see the new state while the previous version is still on disk.
- **Impact:** On crash between the state update and persist completion, the latest
  write is lost.
- **Mitigation:** This is an acceptable trade-off for performance. The system is
  designed to recover from persisted state on restart. Services that fail to persist
  will be reconciled within 30s by the macvlan, DHCP, and router reconcile loops.
- **Introduced:** Original implementation.
- **Status:** Documented trade-off — no fix planned.

### 3. Interface name collision risk (SHA1 truncated to 32 bits)

- **File:** `ingress/internal/macvlan/mac.go:23-32`
- **Root cause:** `InterfaceName` uses SHA1 truncated to 8 hex characters (32 bits,
  ~4 billion values).
  While sufficient for homelab scale, hash collisions are theoretically possible
  with hundreds of thousands of services.
- **Impact:** A collision silently overwrites an existing interface's state.
  Both services would share the same macvlan interface.
- **Mitigation:** Increased hash suffix from 6 to 8 hex characters (24 → 32 bits).
  Collision probability is negligible for homelab deployments (<1000 services).
- **Introduced:** Original implementation.
- **Fixed in:** `ingress/internal/macvlan/mac.go:13` (hashSuffixLen 6→8).
- **Status:** Mitigated — sufficient for target scale.

---

## New Issues Identified

### P3 — Dead Code

#### 4. `endpointIPsForService` is unused (superseded by `endpointInfoForService`)

- **File:** `controller/internal/controller/service_controller.go:213-239`
- **Root cause:** This function was the original implementation for collecting
  endpoint IPs. It was superseded by `endpointInfoForService` which does the same
  work but also collects named port mappings from EndpointSlices.
  The function had zero incoming calls — it was completely orphaned.
- **Impact:** Dead code increases maintenance burden and confuses readers.
- **Fix:** Deleted `endpointIPsForService` — it was no longer referenced anywhere
  in the codebase.
- **Introduced:** Original implementation, orphaned when `endpointInfoForService`
  was added.
- **Status:** **Fixed** — function removed.

---

### P3 — Test Coverage Gaps

#### 5. No unit tests for `auth` package

- **File:** `ingress/internal/auth/` (no `*_test.go` files)
- **Impact:** The OIDC authentication interceptors are critical security code with
  no automated tests. Token validation, subject authorization, and error handling
  are untested.
- **Fix:** Add tests for:
  - `NewInterceptors` with valid/invalid tokens
  - Subject allowlist enforcement
  - Audience validation
  - Missing/malformed authorization headers
- **Introduced:** Original implementation.
- **Status:** Test coverage needed.

#### 6. No unit tests for `dhcp` package

- **File:** `ingress/internal/dhcp/` (no `*_test.go` files)
- **Impact:** DHCP client logic (lease acquisition, renewal, release) has no unit
  tests. The `realClient.run()` loop and retry logic are untested.
- **Mitigation:** Integration testing occurs via controller tests, but unit tests
  would catch regressions faster.
- **Fix:** Add tests for:
  - `Manager.reconcile()` — creates/removes clients based on state
  - `Manager.handleUpsert/Down()` — service mapping logic
  - Pure helper functions (if any exist)
- **Note:** `realClient` requires Linux + network access for full testing; use
  interface mocks for unit tests.
- **Introduced:** Original implementation.
- **Status:** Test coverage recommended.

#### 7. No unit tests for `service` package (gRPC server)

- **File:** `ingress/internal/service/` (no `*_test.go` files)
- **Impact:** The gRPC server methods (`CreateLoadBalancer`, `UpdateLoadBalancer`,
  `GetLoadBalancer`, `Health`) have no unit tests. Error handling and state
  mutations are untested.
- **Fix:** Add tests for:
  - CRUD operations on load balancers
  - Nil pointer handling (already fixed, but should be tested)
  - `ActiveMacvlanFunc` callback integration
  - Invalid argument errors
- **Introduced:** Original implementation.
- **Status:** Test coverage recommended.

#### 8. No unit tests for `state` package

- **File:** `ingress/internal/state/` (no `*_test.go` files)
- **Impact:** Core state management (Subscribe/notify pattern, persistence,
  cloning) has no unit tests. Race conditions in concurrent access would not be
  caught early.
- **Fix:** Add tests for:
  - `Subscribe()` — multiple subscribers receive broadcasts
  - `UpdateService()` / `DeleteService()` — notifications sent correctly
  - `Clone()` — deep copy semantics
  - `Store` — save/load/delete operations
- **Introduced:** Original implementation.
- **Status:** Test coverage recommended.

#### 9. No unit tests for `router/nftables` package

- **File:** `ingress/internal/router/nftables/` (no `*_test.go` files)
- **Impact:** NFTables rule generation and reconciliation logic is untested.
  Complex functions like `rulesNeedUpdate` (complexity: 17) and `AddRules` are
  high-risk for regressions.
- **Mitigation:** Integration testing via controller tests provides some coverage.
- **Fix:** Add tests for:
  - `rulesNeedUpdate()` — rule comparison logic
  - `chainName()` — naming conventions
  - `validateService()` — input validation
  - Use nftables mock or stub for rule generation tests
- **Introduced:** Original implementation.
- **Status:** Test coverage recommended.

---

### P3 — Code Quality Observations

#### 10. High cyclomatic complexity in `rulesNeedUpdate`

- **File:** `ingress/internal/router/nftables/nftables.go:246-316`
- **Complexity:** 17 (HIGH)
- **Impact:** Function is difficult to test thoroughly and prone to bugs. Compares
  desired nftables rules against existing rules across multiple dimensions.
- **Fix:** Consider decomposing into smaller helper functions:
  - `checkIngressRules()` — validate DNAT chain
  - `checkForwardRules()` — validate forward chain
  - `checkLBMaps()` — validate load balancer sets
- **Introduced:** Original implementation.
- **Status:** Refactoring recommended but not critical.

#### 11. High cyclomatic complexity in `endpointInfoForService`

- **File:** `controller/internal/controller/service_controller.go:247-315`
- **Complexity:** 21 (VERY HIGH)
- **Impact:** Function handles EndpointSlice enumeration, named port resolution,
  and Pod fallback logic. Complex branching makes it hard to test all paths.
- **Fix:** Consider extracting:
  - `collectEndpointIPs()` — IP collection from EndpointSlices
  - `collectNamedPorts()` — port name → number mapping
  - `fallbackToPods()` — Pod-based IP collection
- **Introduced:** Original implementation.
- **Status:** Refactoring recommended but not critical.

#### 12. `realClient` struct has 12 fields (potential God Object candidate)

- **File:** `ingress/internal/dhcp/real.go:18-31`
- **Fields:** 12 (iface, mac, ip, mask, lease, acquired, cancel, done, started, onChange, logger, mu)
- **Impact:** While all fields are necessary for DHCP client operation, the struct
  manages connection state, lease state, and lifecycle coordination. This is
  acceptable for a client implementation but should be monitored.
- **Status:** Acceptable design — no action needed.

---

## Resolved Issues (Reference)

The following issues were identified in earlier reviews and have been fully resolved:

| Issue | Severity | Description | Resolution |
|-------|----------|-------------|------------|
| 1 | P0 | Shared unbuffered channel | `State.Subscribe()` / `notify()` broadcast pattern |
| 2 | P0 | `GetLoadBalancer` nil-pointer | Nil checks on `DHCPAcquired` and `Interface` |
| 3 | P0 | CLI port parsing bug | Multiple `Destination` entries per port mapping |
| 4 | P1 | No macvlan reconciliation | 30s reconcile loop in `NetlinkManager.Run()` |
| 5 | P1 | Optimistic state write | `break` on `Create()` error before state mutation |
| 7 | P2 | Create/Delete name inconsistency | Both use `InterfaceName(serviceKey)` |
| 8 | P2 | `Health` reports stale count | `ActiveMacvlanFunc` callback to `NetlinkManager.ActiveCount()` |
| 10 | P3 | Hash collision risk | Hash suffix increased to 8 hex chars |
| 11 | P3 | No macvlan tests | `mac_test.go` with 9 tests |
| 12 | P3 | README `LinkManager` fiction | Updated to document `NetlinkManager` methods |

---

## Summary by Severity

| Severity | Open | Resolved | Status |
|----------|------|----------|--------|
| P0       | 0    | 3        | All critical issues fixed |
| P1       | 0    | 2        | All major issues fixed |
| P2       | 1    | 2        | One by-design behavior documented |
| P3       | 9    | 5        | 1 trade-off documented, 1 dead code removed, 8 new issues identified |

**Overall System Health:** The ingress system is production-ready for homelab deployments.
All critical and major issues have been resolved. Remaining open items are:

- **2 documented trade-offs** (lock coordination, hash collision risk) — acceptable for target scale
- **5 test coverage gaps** — auth, dhcp, service, state, router/nftables packages lack unit tests
- **2 complexity concerns** — `rulesNeedUpdate` and `endpointInfoForService` are complex but manageable
- **1 struct observation** — `realClient` field count is acceptable

**Recommended Next Actions:**
1. Add unit tests for `auth` package (security-critical)
2. Add unit tests for `state` package (core infrastructure)
3. Consider refactoring high-complexity functions during next maintenance window
