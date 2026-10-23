# ADR 0007: Per-client new-connection rate limiting with nftables meters

**Status:** Accepted  
**Date:** 2026-10-23

## Context

The ingress exposes arbitrary TCP/UDP services to the internet. Operators need
a way to stop a single client (or a small number of clients) from exhausting a
Service: accidental loops, credential-stuffing bots, and trivial SYN floods. A
single aggregate bucket per Service is easy to implement but lets one noisy
client starve every other client, so the limit must be **per source IP**.

nftables has no per-IP token bucket primitive in the rule language. The
supported construct is a dynamic set whose elements carry a stateful `limit`
expression — the replacement for iptables `hashlimit` / nft `meter`.

## Decision

- **Scope:** one bucket per `(Service, source IP)`, shared across all ports of
  the Service. Only `ct state new` packets are counted, so established
  connections are never throttled. For UDP this counts original-direction
  packets until the first reply, which is the closest stateless equivalent.
- **Configuration:** two Service annotations,
  `homelab-ingress.vranyes.com/rate-limit` (sustained new connections/second)
  and `.../rate-limit-burst` (token-bucket capacity, defaults to the rate).
  The controller normalises them once and forwards a `RateLimit` proto message.
  Malformed values are logged and the router's previous limit is preserved.
- **Mechanism:** a per-service dynamic set
  `rl_<safeName(serviceKey)>_<rate>_<burst>` (`Dynamic`, `HasTimeout`, 10 minute
  idle timeout, size 65536). The prerouting chain gets:

  ```text
  ip daddr <extIP> ct state new ip saddr
      update @rl_... { ip saddr limit rate over <rate>/second burst <burst> }
      counter drop
  ```

  `Over: true` inverts the limit match: under the limit the dynset breaks out of
  the rule (traffic continues to geo/DNAT); over the limit it falls through to
  the counter and drop.
- **Ordering:** geo deny → rate limit → DNAT → final daddr drop. Geo-denied
  sources must not consume set slots. The rate-limit drop must precede DNAT
  because the DNAT branch ends in `accept`, which terminates the chain.
- **Live changes:** the kernel clones the limit expression into every set
  element. Existing elements therefore keep their old bucket until they expire.
  Including `(rate, burst)` in the set name makes a configuration change
  materialise a fresh, empty set; the previous set is deleted in the same
  batch. Reconciles that do not change the limit reuse the existing set, so
  buckets survive.
- **Cleanup:** a missing `rate-limit` deletes the set. Deleting a set is only
  attempted after `GetSets` confirms it exists: `DelSet` on a missing set
  returns ENOENT and aborts the whole netlink batch, which would leave the
  Service with no rules at all.
- **Failure mode:** at set capacity (65536 tracked IPs) the dynset insert
  fails and the non-inverted path breaks out of the rule, so the Service fails
  open. This is deliberate: dropping all new traffic because the tracking table
  is full turns a resource limit into an outage.
- **Invalid state:** `validateService` rejects a zero rate or zero burst.
  `AddRules` returns before flushing, so the Service keeps its previous rules.

## Consequences

- No new dependency: `github.com/google/nftables` already supports
  `expr.Dynset` with per-element `expr.Limit`.
- One extra rule and one dynamic set per rate-limited Service. Sets are bounded
  by the 10 minute timeout and the 65536-element cap.
- `GetLoadBalancer`/`ListLoadBalancers` now carry the normalised limit, and the
  controller compares it on every reconcile.
- The e2e suite pins enforcement, refill, live update, removal and per-client
  isolation; unit tests pin the expression layout and set naming.
