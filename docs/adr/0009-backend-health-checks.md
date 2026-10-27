# ADR 0009: Active TCP backend health-checking with fail-open filtering

**Status:** Accepted  
**Date:** 2026-10-27

## Context

The controller only forwards Ready endpoints, but that covers what the API
server reports. Out-of-band failures still route into black holes: a stale
endpoint IP, a firewall change on a node, or a crashed pod whose slice entry
has not been removed yet. The nftables DNAT maps (`map_<sn>_<port>`) spray
traffic across every listed backend, so one dead backend drops a share of
connections until the next state event rebuilds the rules.

## Decision

- **New package `internal/health`:** a `Checker` snapshots `State` every
  `Interval` (default 10s) and TCP-dials each backend with `Timeout`
  (default 2s). Per-backend hysteresis: `FailThreshold` consecutive failures
  (default 2) mark it down, `SuccessThreshold` consecutive successes
  (default 2) mark it up again. Staggered single failures never flap.
- **UDP is never probed and always healthy.** UDP has no handshake, so a dial
  cannot distinguish "listening" from "black hole"; probing it would only
  produce noise. Unknown (never-probed) backends also report healthy, so rule
  planning fails open until the first probe completes.
- **Configuration:** an optional `healthcheck:` block (`interval`, `timeout`,
  `fail_threshold`, `success_threshold`); unset fields take the defaults.
- **Wiring:** the nftables `Manager` takes an optional `HealthChecker` via
  `WithHealthChecker` (same pattern as `WithGeoLookup`). `planRules` filters
  each target's TCP destinations to healthy backends before building the
  DNAT map. The filter is in-memory at rule-build time only: persisted State
  keeps the full backend list.
- **Fail-open:** if filtering would empty a target, the original full set is
  kept and a `health_all_down` warning is logged. A checker outage (or a
  short correlated backend outage) degrades to "try everything" instead of
  black-holing the service.
- **Convergence:** health flips invoke an `OnChange` hook wired to
  `router.Manager.Reconcile`, so DNAT maps converge between state events
  (state events and the 5-minute reconcile also rebuild from current health).
  The checker shares the daemon ctx, so shutdown cancels its ticker cleanly.
- **Observability:** `homelab_ingress_backend_up{service,addr,port}` (1/0) and
  `homelab_ingress_backend_probes_total{service,result}`; `Checker.Stats()`
  exposes the same snapshot for non-Prometheus consumers. The all-down
  fail-open is log-only (no dedicated counter).

## Consequences

- An extra TCP connection per backend per interval: negligible next to real
  traffic, but operators with connection-counting backends should know.
- A backend that refuses the probe port while serving traffic through another
  path (e.g. a sidecar) is treated as down; probes target the exact
  destination `address:port` from State, so this only misfires when the
  backend genuinely does not accept that socket.
- No proto/state schema change: health is daemon-local derived state, never
  persisted and never sent to the controller.
