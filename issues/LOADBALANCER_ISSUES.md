# Loadbalancer: Identified issues (updated 2026-05-05)

This document lists security, correctness, and performance issues discovered during a code review of ./loadbalancer. Items marked RESOLVED reflect problems that appear to have been fixed in the current code; remaining items are actionable and prioritized.

PRIORITY: HIGH

1) Missing TLS for gRPC when authentication is enabled (SECURITY, HIGH)

- Files: cmd/loadbalancer/main.go, internal/config/config.go
- Location: gRPC server is created without TLS options; config has no TLS cert/key fields.
- Problem: the code supports OIDC bearer tokens (auth interceptors) but sends tokens over plaintext TCP by default. Tokens and other metadata can be eavesdropped or replayed.
- Remediation: add TLS support to the server (config fields for cert/key and optional client CA), require or strongly recommend TLS when `auth.issuer` is set, and use `grpc.Creds` to configure server credentials. Consider adding TLS-related validation to startup.

1) CLI and local tools use plaintext gRPC (SECURITY, HIGH)

- Files: cmd/cli/main.go
- Location: dial() uses `grpc.DialContext(..., grpc.WithTransportCredentials(insecure.NewCredentials()))` unconditionally.
- Problem: the CLI sends control calls (and bearer tokens if used) over plaintext.
- Remediation: add CLI flags to enable TLS and to pass the CA/cert/key or trust store; default to secure transport for non-local addresses. Document safe local development (e.g., use SSH port-forwarding) versus production.

1) UpdateLoadBalancer applies firewall rules without matching external IP (FUNCTIONAL/SECURITY, HIGH)

- Files: internal/service/router.go
- Location: UpdateLoadBalancer calls `fwMgr.AddServiceRules(iface, "", fwPorts, req.ServiceKey)` (extIP is empty).
- Problem: omitting extIP causes DNAT rules that do not match the destination IP and therefore may apply broadly (matching any destination IP) — this can cause unintended NAT of unrelated traffic.
- Remediation: require a non-empty extIP when installing DNAT rules, or use the service's current assigned IP (svc.CurrentIP) when present. Validate extIP before applying rules.

PRIORITY: MEDIUM

1) DHCP assignIP removes addresses indiscriminately (ROBUSTNESS, MEDIUM)

- Files: internal/dhcp/real.go
- Location: assignIP() removes every other IPv4 address on the interface when assigning the new IP.
- Problem: interfaces may legitimately carry multiple addresses; aggressively deleting other addresses can disrupt unrelated services.
- Remediation: only remove addresses that conflict (same network/host) or that were previously created by this service (track assigned addresses), or make removal configurable.

1) State directory permissions are permissive (SECURITY/INFO, MEDIUM)

- Files: internal/state/store.go
- Location: Save() uses `os.MkdirAll(filepath.Dir(s.path), 0755)` when creating parent directory.
- Problem: the state directory is world-readable though the state file is created 0600. Tighten directory permissions to 0700 (or configurable) to reduce information disclosure risk.

PRIORITY: LOW / STYLE / PORTABILITY

1) Authorization header parsing is brittle (LOW)

- Files: internal/auth/auth.go
- Location: verifyAndInject() extracts bearer token using a case-insensitive prefix check then slices the original string.
- Remediation: parse header with `strings.Fields` or `strings.SplitN(strings.TrimSpace(v), " ", 2)` and validate the token part robustly.

1) Protocol mapping defaults to TCP (BEHAVIORAL, LOW)

- Files: internal/firewall/nftables.go
- Location: matchProtoPortExprs(): any non-`"udp"` value is treated as TCP.
- Remediation: validate allowed protocol strings and return an error for unsupported protocols.

1) NFTables endianness/packing (PORTABILITY, LOW)

- Files: internal/firewall/nftables.go
- Location: addLBMapNamed(): keys are encoded with native endian while port bytes use big-endian encoding. Document expected endianness or use consistent encoding.

1) On-disk state version not validated (ROBUSTNESS, LOW)

- Files: internal/state/store.go, model.go
- Location: Load() unmarshals saved state but does not check `Version` for compatibility.
- Remediation: add a version check and migration or explicit failure if incompatible.

1) Logging of MACs/IPs/ports (PRIVACY)

- Files: various (dhcp, recovery, macvlan, firewall)
- Location: multiple log statements include MAC and IP values.
- Remediation: consider configurable log redaction levels for sensitive deployments.

RESOLVED / OBSERVED FIXES

- DHCP nil pointer deref (previously reported): RESOLVED — code now uses a nil-safe comparison before calling `Equal` in `internal/dhcp/real.go`.
- UpdateService nil MAC panic: RESOLVED — UpdateLoadBalancer captures/parses existing MAC and generates a new one if parsing fails before calling `state.UpdateService` (internal/service/router.go).
- Recovery recreating chain per-port: RESOLVED — recovery now uses `AddServiceRules` (single call) to add all ports atomically.
- nftables set flush behavior: RESOLVED — `removeServiceChain` flushes set deletions as part of removal.

RECOMMENDED NEXT STEPS

1. Implement TLS support for the gRPC server, require TLS when `auth` is configured, and add CLI flags to use TLS. (High priority)
2. Fix UpdateLoadBalancer to pass the service's current assigned IP when available (or validate extIP) before installing DNAT rules. (High priority)
3. Adjust DHCP assignIP to avoid removing unrelated addresses or make removal behavior configurable. (Medium)
4. Tighten state directory permissions to 0700. (Medium)
5. Add unit tests covering firewall rule installation (extIP present vs absent), DHCP assignIP behavior with multiple addresses, and auth interceptor parsing.

If you'd like, I can prepare a PR implementing the high/critical fixes (1 and 2 above) and follow up with the DHCP/address behavior change. Which fix should be implemented first? (Suggested: enable TLS and update CLI, then fix UpdateLoadBalancer extIP handling.)
