# ADR 0001: Geo-enforcement via deny-before-DNAT with per-service union sets

**Status:** Accepted  
**Date:** 2026-08-06  
**Grills:** #1 (inverted allow/deny → deny-first), #1a (per-service union set vs N-lookup)

## Context

The original implementation plan (preserved in git history) placed `ip saddr @geo_<cc> counter accept` rules before the DNAT rules. In nftables a nat-prerouting `accept` verdict terminates chain evaluation; the matching packet never reaches the DNAT rules. This inverts the policy:

- Allowed-country traffic is `accept`ed before DNAT → no DNAT → packet routed locally to the macvlan child IP (INPUT) → no listener → broken.
- Disallowed-country traffic falls through to the DNAT rule (which has no source predicate) → DNAT'd → forwarded to backend → allowed.

The fix: use a deny-first rule that drops sources not in the allow list before DNAT ever runs.

A second question was whether to share per-country sets (one `geo_US` referenced by multiple services) or materialise a union set per service. Both achieve the packet-level goal; the trade-off is kernel-memory duplication vs no per-service set-GC burden.

## Decision

1. **Deny-before-DNAT.** The first rule in each `pre_<svc>` chain is:
   ```
   ip daddr <extIP> ip saddr not in @geo_<svc> counter drop
   ```
   Allowed sources don't match (source IS in the set, so `not in` is false) → fall through to DNAT.  
   Disallowed sources match → drop before DNAT. This gives the security property Spec B requires.

2. **Per-service union sets, not shared per-country sets.** Each `pre_<svc>` chain references its own `geo_<safeName(serviceKey)>` set containing the union of CIDRs from all allowed countries for that service.
   - GeoLite2-Country CIDRs are globally disjoint across countries, so the union is a plain concatenation — no overlap, no kernel `-EEXIST` on insert.
   - Sets named `geo_<sn>` are automatically cleaned up by the existing `RemoveRules` loop (which deletes sets whose name contains the service-key hash). No set-GC subsystem needed.
   - The geo.Manager still caches the data per country (`countryCode → []*net.IPNet`); only the kernel materialisation is per-service.

3. **`buildGeoSetDenyExprs(c, table, extIP, cidrs)`** replaces the plan's `geoCIDRSetFor`. It creates/flushes `geo_<sn>` and returns expressions for the single `not in` rule.

## Consequences

- **Plan changes:** §4.6 must replace `buildGeoSetAcceptExprs` with `buildGeoSetDenyExprs`, and the AddRules flow inserts one deny rule (not N accept rules). `RefreshGeoSets` flushes the per-service union sets individually.
- **Spec A** changes: asserts the deny-rule counter stays zero and the DNAT counter increases (not a geo-accept counter). Must also assert backends are reachable (e.g., `CollectBackendIDs` succeeds) — the original Spec A lacked this check.
- **Spec D** reworded: proves per-service *isolation* via drop/DNAT counter deltas, not via shared-set references. The isolation property is the real requirement.
- **Kernel memory:** Per-service union sets duplicate elements. At homelab scale (single-digit services, 1–3 countries each, ~100k elements per big country → ~6 MB per US set) this is acceptable. Larger deployments should revisit.
- **No other grilling items** are affected by this decision.

## Alternatives considered

### Shared per-country sets with N lookups
```
ip daddr <extIP> ip saddr != @geo_US ip saddr != @geo_CA counter drop
```
One rule with N `Lookup{Invert: true}` expressions. Sets materialised once globally. Eliminates per-service memory duplication but requires set-GC (delete `geo_US` when the last referencing service is removed). Rule length grows with allowed-country count. Rejected in favour of simpler code (no GC) and constant rule length.

### Original plan (accept-before-DNAT)
Rejected — verdict `accept` in nat prerouting terminates chain evaluation; allowed traffic never reaches DNAT, disallowed traffic does. Inverts the policy.