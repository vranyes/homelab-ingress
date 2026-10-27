# ADR 0008: Denied CIDRs and per-service load-balancing policy

**Status:** Accepted  
**Date:** 2026-10-27

## Context

Two gaps in per-Service traffic control:

1. The geo allow-list (ADR 0001) plus static `allowed-cidrs` can only express
   "allow these". Operators also need "deny these even when allowed": e.g.
   block a hostile netblock that sits inside an allowed country or an allowed
   LAN range, without shrinking the allow-list.
2. Backend selection is hardcoded to round-robin (`numgen inc`). Some
   Services (stateful sessions, uneven backends) want source-IP stickiness or
   random spread instead.

## Decision

- **Denied CIDRs:** new `homelab-ingress.vranyes.com/denied-cidrs`
  annotation (IPv4, comma-separated, canonicalised via `IPNet.String()`,
  de-duplicated). The daemon materialises a per-service interval set
  `deny_<safeName(serviceKey)>` and installs as the **first** rule in the
  chain:
  ```text
  ip daddr <extIP> ip saddr in @deny_<sn> counter drop
  ```
  Ordering is deny → geo deny → rate limit → DNAT → final drop, so a denied
  source never consumes rate-limit slots or reaches DNAT. The set name embeds
  the service hash, so the existing `RemoveRules` prefix-GC deletes it.
  Parsing, validation, and fail-closed last-good preservation mirror
  `allowed-cidrs` exactly (IPv6 rejected: the table is IPv4-only).
- **LB policy:** new `homelab-ingress.vranyes.com/lb-policy` annotation
  (`round-robin` default, `hash-source-ip`, `random`; normalised
  lowercase/trim, unknown values keep the last-good policy). Proto carries
  `lb_policy` (empty = default). All policies share the same
  index → backend map layout; only the index selection differs:
  - `round-robin`: `numgen inc mod N` (unchanged behaviour).
  - `random`: `numgen random mod N`.
  - `hash-source-ip`: Jenkins hash over the 4 source-address bytes, `mod N` —
    one client IP sticks to one backend without per-connection state.
  Switching policy reuses the existing map (same elements), so no traffic
  drops during the change.
- **Non-goal:** `least-conn` is explicitly not implemented. nftables is
  stateless here — it cannot count active connections per backend — so a
  least-connections policy cannot be enforced in the data plane.

## Consequences

- One extra interval set and one extra rule per Service that sets
  `denied-cidrs`; none otherwise.
- `Get/Update/CreateLoadBalancer` carry `denied_cidrs` and `lb_policy`; the
  controller compares both as sets/values on every reconcile.
- Unit tests pin deny-first ordering, the non-inverted deny match, and each
  policy's expression pipeline shape.
