# ADR 0005: Implementation seams and the EU source address

**Date:** 2026-10-20
**Status:** Accepted
**Supersedes:** ADR 0002, only the `ns-external` topology (the CIDR carve-up is
unchanged).

## Context

ADR 0001–0004 fixed the policy (deny before DNAT, per-service union sets), the
database lifecycle (optional MaxMind, deferred validation), and the rebuild
callback (`RefreshGeoSets`). They left three implementation choices open that
matter for testability and for the e2e VM:

1. ADR 0002 proposed a dedicated `ns-external` namespace and veth pair for
   EU-sourced traffic. The current VM topology already has a VEPA peer
   (`ns-server`) whose reflective relay carries traffic from the bridge. Adding
   a whole namespace means another veth pair, another bridge port, and another
   MAC-learning surface.
2. Rule ordering (`geo deny → DNAT → final drop`) is security-critical, but
   integration tests could only observe it by decoding netlink messages.
3. The geo cache needs a testable boundary between the MaxMind reader, the
   filesystem watcher, and nftables.

## Decision

### 1. EU traffic uses a secondary address on `ns-server`

Specs that need an EU-mapped source add `10.44.0.129/24` to `veth-outer` inside
`ns-server` (`ip -n ns-server addr replace ...`) and bind curl with
`--interface 10.44.0.129`. Cleanup removes the address. The mock carve-up
(`10.44.0.128/26 = EU`) is unchanged.

Consequences:

- One bridge port, one relay, no extra namespace lifecycle.
- The secondary address is visible in `ns-server`, so it is a strictly
  additive change even on a kept VM.
- Spec 2's flip from `US` to `EU` doubles as the control that the source
  binding actually traverses prerouting: EU traffic is blocked while US is
  allowed, then allowed while US is blocked.

### 2. nftables gets a small, explicit test seam

- `setStore` is the four-method subset of `*nftables.Conn` used for geo sets
  (`GetSets`, `AddSet`, `FlushSet`, `SetAddElements`). A fake records
  create/flush/element calls.
- `planRules(conn, setStore, table, chain, svc)` returns the ordered rules
  without adding them, so unit tests assert `geo-deny → DNAT → drop` directly.
- `refreshGeoSets(setStore, table, svcCIDRs)` is the set-only half of
  `RefreshGeoSets`.
- `errTableNotFound` lets an early geo reload be a no-op before the first
  service has created the table.

### 3. `geo.Manager` depends on a `Source` interface

```go
type Source interface {
    Reload() error
    CountryCIDRs(cc string) ([]*net.IPNet, error)
    LookupCountry(net.IP) (string, bool)
}
```

`geoip.Manager` implements it; geo tests use a fake. `geo.Manager` watches
the parent directory (rename-safe), filters events for the configured path, and
polls file metadata as a fallback. A failed `Reload` keeps the previous reader
and does not fire `OnRebuild`.

### 4. Mock mode is selected by `provider: "mock"`

The TSV reader is chosen before any network access. `BeforeSuite` rewrites the
fixture atomically from the canonical carve-up so a crashed prior run cannot
poison the suite. (The older `license_key: "mock"` spelling is still accepted;
see ADR 0006.)

## Alternatives considered

- **Keep `ns-external`:** rejected — more moving parts than the property needs,
  and the relay already exists.
- **Test rule order through netlink decoding:** rejected — brittle and would
  couple tests to attribute encoding.
- **Let geo call nftables directly:** rejected by ADR 0004; the callback keeps
  the dependency arrow one-way.
