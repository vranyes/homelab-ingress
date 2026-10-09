# ADR 0003: MaxMind block optional at startup, geo request rejected at RPC layer

**Status:** Accepted  
**Date:** 2026-08-06  
**Grill:** #3 (maxmind.license_key required breaks backward compat)

## Context

The original plan required `maxmind.license_key` at startup — error if
empty. Every existing deployment has no `maxmind` block in its config
file, so upgrading would kill the daemon. The plan itself claims
backward compatibility ("empty `allowed_geos` is a no-op"), but that
no-op is unreachable if the daemon refuses to start.

## Decision

- **MaxMind block is entirely optional.** If absent or `license_key` is
  empty, the daemon starts normally with geo-blocking disabled.
- **Geo requests without MaxMind return `InvalidArgument`** at the
  gRPC layer — `CreateLoadBalancer`/`UpdateLoadBalancer` with non-empty
  `allowed_geos` returns `codes.InvalidArgument` instead of silently
  ignoring the field.
- **The controller's existing error handling** treats `InvalidArgument`
  as non-retryable (since `router.IsRetryable` checks only
  `Unavailable`/`DeadlineExceeded`/`ResourceExhausted`). The reconcile
  fails, surfaces in controller logs, and the next watch event
  re-triggers once MaxMind is configured.

## Consequences

- **Backward compatibility is preserved.** No existing deployment needs
  a maxmind block.
- **Fail-closed at the correct layer.** The error surfaces when an
  operator actively tries to use geo (applying the annotation), not at
  every daemon startup. No silent misconfiguration.
- **No controller code changes** beyond the existing geo plumbing
  (plumbing `allowed_geos` through `Create`/`Update` signatures).
  The error path is inherited from the existing `IsRetryable` / error
  return pattern.
- **The startup validation block** in §4.1 no longer contains a hard
  error on missing license_key; it falls through to a no-op state.
- **e2e harness** always sets `maxmind.license_key: "mock"`, so the
  test environment never exercises the "no MaxMind" path. That path is
  exercised implicitly by all existing (pre-geo) e2e specs, which send
  no `allowed_geos` and thus never hit the validation.