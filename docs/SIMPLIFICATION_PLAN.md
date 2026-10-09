# Ingress Service Simplification Plan

## Architecture Overview

The ingress service (`ingress/`) is a Linux-only L4 load balancer daemon with 9
internal packages and a CLI (`hictl`). Its functional contract (preserved):

- **gRPC `RouterService` proto** — 6 RPCs (Create/Update/Delete/Get/List/Health)
- **Config YAML shape** — `base_interface`, `grpc`, `ws`, `auth`, `masquerade`
- **External behavior** — macvlan interface provisioning, DHCP lease acquisition,
  nftables DNAT round-robin rules, masquerade, OIDC auth, persisted crash recovery

The e2e suite (10 Ginkgo specs in a lima VM: 0/1/2 LB, churn, OIDC reject,
masquerade) is the validation gate.

## What's Driving Complexity Today

| # | Smell | Evidence |
|---|---|---|
| 1 | **3 concurrent managers race on shared state** | macvlan, dhcp, and router each `Subscribe()`, `Clone()`, react to `LBNotification`, and call `st.UpdateService()` which re-broadcasts. Butterfly effect: Create → macvlan updates svc → DHCP handles update → DHCP updates svc again → router sees both. Race-prone and hard to reason about. |
| 2 | **Pub/sub + in-memory state + SQLite store conflated** | `State` mixes concerns: subscriber fanout, locking, persistence proxy, lease store proxy. `Store` duplicates locking. The ISSUES.md P3 #2 calls out uncoordinated locks. |
| 3 | **SQLite is sledgehammer for ~20 rows** | `modernc.org/sqlite` pulls in libc, libsqlite, mathutil, memory — ~30 transitive modules. State is small and JSON-serializable already. |
| 4 | **`LBService.Clone()` is a 40-line manual deep-copy** | `state.go:46-89` — high cyclomatic (10), manual per-field clone of pointers/slices/net types. Bug-prone. |
| 5 | **Auth package duplicated between ingress, controller, e2e harness** | `tokenFileCredentials` exists 3× (hictl, controller, harness). `ExtractBearerToken` + HTTP variant in `auth.go`. CLI has its own `tokenCredential` copy. |
| 6 | **hictl is 552 lines in one file** | Repeated dial/error boilerplate in 6 subcommands. Cobra + viper + manual flag binding. Mixed positional/flag error handling (`os.Exit(1)` everywhere). |
| 7 | **macvlan vs router vs DHCP each have their own reconcile ticker + event loop** | Same `select { event; ticker }` pattern 3×. Each at 30s. Each clones state, computes desired-vs-active, differs slightly. |
| 8 | **`safeName(serviceKey)` vs `InterfaceName(serviceKey)` create two different IDs** | nftables uses md5-hex prefix + sanitized 32-char; macvlan uses sha1-hex suffix + 8-char hash. Two naming schemes for the same logical service — fragile, hard to test. |
| 9 | **41 orphans + 5 untested-but-called production functions** | gograph orphans scan shows half of `state/*`, all of `auth/*` exports, all of `certgen/*`, all of `nftables/*` builders are unreachable from any detected entry point (gograph AST mode doesn't see `main()` on linux build). Realistically many are reachable via `main.go`, but `(*State).Subscribe`, `(*TokenVerifier).Verify`, `(*LBService).ParseMAC`, `SubjectFromContext` etc. are genuinely dead. |
| 10 | **`router.Manager.Reconcile()` redundantly re-applies everything every 5 minutes** | `interface.go:76-80` calls `AddRules` for *all* services on every tick — nftables flushes happen for unchanged services. |
| 11 | **`netlink` + `nftables` libraries assume singleton table/conn** | `Manager.NewManager` deletes the table on startup; `AddRules` flushes chains. No idempotence at the rule level — heavy reuse of `findTable`, `ListChains` (one per AddRules call!) |
| 12 | **Static `validateService` lives in `nftables/helpers.go` but is also called by the stub** | `nftables_stub.go:18` imports from `state` only to call `validateService` — coupling stub → helpers → state. |

## Proposed Simplifications (Phased, Incremental)

### Phase 1 — Foundation (low risk, high value)

**1a. Replace SQLite with JSON file persistence**
- Replace `state/store.go` with a 2-file (`services.json`, `leases.json`)
  atomic-write store backed by `os.WriteFile` + `os.Rename`.
- Drop `modernc.org/sqlite` and its ~30 transitive deps from `go.mod`. Binary
  size shrinks materially.
- Eliminates the dual-lock issue documented in ISSUES.md P3 #2 — only
  `State`'s mutex remains.
- Same `LoadServices/SaveService/DeleteService/LoadLeases/SaveLease/DeleteLease`
  API, so `State` and `main.go` don't change beyond wiring.
- E2e behavior identical (state restored on restart).
- Risk: SQLite WAL/ACID guarantees gone — acceptable since reconciliation within
  30s is already documented policy.

**1b. Delete dead code**
- `(*LBService).ParseMAC` (returns field, never used) — `state.go:156`
- `SubjectFromContext` (returns ctx key, never read) — `auth.go:32`
- `buildMasqueradeExprs` (unused; only the *WithDevices variant is called) —
  `exprs_bldrs.go:88`
- `(*NetlinkManager).Exists`, `GetMAC` if not used outside tests
- `(*wrappedStream).Context` only has 1 internal caller — keep
- Verify each deletion with `gograph_callers` before removal; safe because e2e
  is the safety net.

**1c. Unify the two service-name generators**
- Collapse `nftables.safeName` and `macvlan.InterfaceName` to a single
  `state.ServiceKey{Interface, Chain, Set}` derivation function in
  `state/identity.go`. Both packages call it.
- Removes bug class: chain != interface mismatch trivially.

### Phase 2 — Collapse the manager triad

**The big one.** Merge macvlan + dhcp + router managers into a single
**`internal/lifecycle` package** with:

- One `Reconciler.Run()` goroutine consuming one `Subscribe()` channel.
- A single 30s reconcile pass that walks the desired state and **idempotently**
  ensures each service's full stack (macvlan → DHCP → nftables DNAT →
  masquerade entry) in dependency order.
- Drops 3 goroutines → 1, drops 3 reconciler tickers → 1, drops 3 separate
  clone-and-compare loops → 1.

Sub-problems this solves:

- Race between macvlan create + DHCP upsert + router apply — gone (per-service
  lock-ordered flow).
- Multi-broadcast of UpdateService triggering 3× the work per logical change —
  gone.
- The explicit ordered-shutdown dance in `main.go:164-176` becomes "stop the
  single reconciler, wait for its drain".
- Bug class where `macvlan.reconcile()` reassigns MAC differently from DHCP's
  expectation — gone.

**Idempotent nftables rewrite.** Today `AddRules` flushes the chain and
re-adds every rule on every call; `Reconcile()` does this for **every**
service every 5 minutes. Replace with:

- Compute desired rule set as a hash and compare against current handles.
- Use `nftables` batch ops to delete/add only the diff.
- Stop calling `DelTable` at startup — instead `DelChain` only the per-service
  `pre_*` chains we no longer want. Idempotent reconciliation means a crash
  leaves things consistent.

### Phase 3 — Consolidate auth & deps

- Move `tokenFileCredentials` into a shared location under
  `ingress/internal/auth` (or `pkg/auth` if the controller needs it too) and
  import it from hictl, e2e harness, controller. Today there are 3 copies
  with byte-identical bodies.
- Collapse `ExtractBearerToken` (gRPC) and `ExtractBearerTokenHTTP` into one
  function with a transport argument, since they differ only in which header
  source to read.
- `auth.NewInterceptors` and `auth.NewTokenVerifier` both create independent
  `oidc.Provider` connections — combine into
  `auth.New(ctx, cfg) (*Verifier, Interceptors)` so the OIDC discovery roundtrip
  happens once.
- Drop `*x509.CertPool` parameter-grouping smell — `NewTokenVerifier` has 6
  args (qualifies in gograph_arity). Replace with an
  `auth.Config{Issuer, Audience, Allowed, TrustedRootPath}` struct.

### Phase 4 — hictl cleanup (free to change)

Options, in increasing aggressiveness:

1. **Keep cobra+viper, factor the boilerplate.** Extract a single
   `dialAndRun(fn)` that handles dialing/defer/error printing. Each subcommand
   shrinks ~40%. 552 lines → ~300. Low risk.
2. **Drop viper, keep cobra.** viper is currently used to read
   `~/.config/hictl/config.yaml`. The flag→env→file precedence could be a
   20-line `os.Getenv` shim. Removes the viper dep + ~30 transitive modules.
3. **Replace cobra with stdlib `flag`.** Saves ~5MB binary. Roughly halves
   line count. Only worth doing if CLI surface stays small (6 subcommands).

Recommend option **1 first**, then evaluate 2 after Phase 3 lands.

### Phase 5 — Struct & API tidy

- Drop `(*State).Clone()` — it returns `map[string]*LBService` shallow with
  deep `Clone()` of each service; only called by 3 reconcilers that won't exist
  post-Phase 2. Replaces with a `state.Snapshot()` returning a
  `map[string]LBService` (value, not pointer) — eliminates 50% of deep-copy
  surface.
- `LBService.Clone()` → add a `//go:generate` deepcopy out-of-tree, or factor
  `net.IP`/`net.HardwareAddr` slices into a `state.bytes` helper since they all
  share "maybe-nil slice with copy" pattern.
- Drop `(*LBService).SetInterfaceName`, `SetMAC`, `ParseMAC`,
  `GetInterfaceName` if Phase 2 inlines them — currently all flagged orphan.
- Merge `service.protoPortsToState`/`statePortsToProto` with
  `controller/internal/router/{toProtoPorts, fromProto}` — they exist 2×, each
  side of the wire. The controller pulls its own copy of the proto
  (`controller/pkg/api/router/v1`) via vendoring — fix the version skew rather
  than mirror functions.

### Phase 6 — Test surface

ISSUES.md flags 4 untested packages. Without unit tests, the various phases
above are blind refactors. Order:

1. **state** (post Phase 1) — pub/sub, Load/Save JSON, Clone/Snapshot.
2. **auth** (pre/post Phase 3) — subject allowlist, audience validation,
   malformed header.
3. **service** (post Phase 2) — gRPC CRUD against in-memory state with a fake
   reconciler.

## Pre-conditions & Risk Per Phase

| Phase | Pre-conditions | Risk | E2E coverage |
|---|---|---|---|
| 1a SQLite→JSON | Read `Store` API | LOW | churn_recreate verifies recovery |
| 1b dead code | gograph_callers verify | LOW | none needed |
| 1c name unify | n/a | LOW | two_lb_test isolates per-LB chains |
| 2 manager merge | Phases 1a, 1c landed | MEDIUM-HIGH | all 10 specs |
| 3 auth dedup | Phase 2 lands first | MEDIUM | oidc_reject_test |
| 4 hictl | Anytime after 1 | LOW | not e2e-tested |
| 5 API tidy | Phase 2 done | LOW | churn_test, two_lb |

## What I'd Cut If Scope Got Tight

The Phase 2 manager merge is the highest-leverage but riskiest. If a safer
subset is needed:

- Do Phase 1a + 1b + 1c first. Stop there: ~30% complexity reduction, ~30
  transitive deps removed, two naming bugs eliminated.
- Phase 3 (auth) is independently shippable.
- Phase 2 is worth it on its own, but should follow the foundation phases by at
  least one release to ensure SQLite removal didn't regress before we
  re-architect concurrency.

## Open Questions Before Implementation

1. **Config YAML shape preservation.** "Config YAML shape" is preserved — can
   we add new optional fields (e.g. `dhcp.renewal_interval`,
   `reconcile_interval`) behind defaults, or strictly no schema additions?
2. **`hictl` binary location.** It currently lives under
   `ingress/cmd/hictl`. Phase 4 changes don't touch the path — confirm.
3. **gograph precision.** For the actual edit phase run
   `gograph build . --precise` so CHA/SSA gives stricter call edges for the
   audit before PRs. Any objections to running `go build` invocations for that?
