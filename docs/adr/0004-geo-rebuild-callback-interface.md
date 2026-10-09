# ADR 0004: RefreshGeoSets callback interface — geo.Manager computes the service→CIDRs map

**Status:** Accepted  
**Date:** 2026-08-06  
**Grill:** #4 (OnRebuild / RefreshGeoSets / wiring mismatches)

## Context

The original plan's three interface points didn't line up across the
geo.Manager → nftables.Manager data path:

1. `RefreshGeoSets(geos map[string][][]*net.IPNet)` — the double-nested
   slice is a typo; the geo.Manager cache is `map[string][]*net.IPNet`
   and per-service concatenation produces `map[string][]*net.IPNet`.
2. `OnRebuild(func())` — fires with no arguments. The nftables layer
   would need to derive `svcCIDRs` itself, but the nftables Manager has
   no `*state.State` reference and doesn't know about per-service
   AllowedGeos.
3. §4.7 wiring referenced `rmgr.RefreshGeoSets`, but `rmgr` is the
   `router.Manager` interface wrapper; `RefreshGeoSets` is defined on the
   concrete nftables `Manager` (`iml`).

## Decision

- **`RefreshGeoSets(svcCIDRs map[string][]*net.IPNet) error`** on the
  nftables `Manager`. Map is keyed by service key (the
  `state.LBService.ServiceName` namespace/name format), value is the
  concatenated CIDRs across all allowed countries of that service.
- **`OnRebuild(func(map[string][]*net.IPNet))`** on geo.Manager —
  typed callback, takes the `svcCIDRs` map as its argument.
- **geo.Manager computes `svcCIDRs` itself** before invoking the
  callback: walks `st.Clone().Services`, picks services with non-empty
  `AllowedGeos`, and for each concatenates `CountryCIDRs(cc)` for every
  allowed country from the freshly-reloaded cache. geo.Manager has both
  required inputs (state reference for active-service list, cache for
  per-country CIDRs), so it's the natural producer.
- **Wiring in `cmd/ingress/main.go`** calls the callback on the concrete
  nftables Manager, not on `router.Manager`:

  ```go
  geoMgr.OnRebuild(func(svcCIDRs map[string][]*net.IPNet) {
      if err := iml.RefreshGeoSets(svcCIDRs); err != nil {
          logger.Error("failed to refresh geo sets", "error", err)
      }
  })
  ```

## Consequences

- `RefreshGeoSets` is a single-pass caller of the existing
  `buildGeoUnionSet` helper: for each `svcKey, cidrs` pair, flush + repopulate
  `geo_<sn>`. No state mutation, no chain rebuild needed (the deny rule
  references the set by `SetID`, which stays valid across FlushSet).
- geo.Manager remains the "controller of truth" for the service→CIDRs
  materialization; the nftables layer is a pure consumer.
- The `SnapshotInUse()` helper on geo.Manager is still used, but only by
  the geo.Manager itself (e.g., for internal logging). The
  `svcCIDRs` calculation does not depend on it directly — it walks state
  and filters by non-empty `AllowedGeos`.
- The callback is synchronous from the geo.Manager's rebuild goroutine
  perspective: a slow nftables RefreshGeoSets blocks the next reload.
  Acceptable at 24h reload cadence.