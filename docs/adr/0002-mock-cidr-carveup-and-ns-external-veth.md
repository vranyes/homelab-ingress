# ADR 0002: Mock CIDR carve-up and ns-external veth topology

**Status:** Accepted  
**Date:** 2026-08-06  
**Grills:** #2 (ns-external route unreachable), #2a (127.0.0.0/8 loopback dropped by kernel)

## Context

Spec B and D require a source IP that maps to a non-allow-listed country
to exercise the geo-deny path. The original plan used `127.0.0.0/8 → EU`
as the mock range and created `ns-external` with only a loopback device.

Two problems surfaced during grilling:

1. **127.0.0.0/8 cannot be used as a source on non-lo interfaces.** The
   Linux kernel (`ip_local_out`) drops packets sourced from the loopback
   range on any interface other than `lo`. A curl from a namespace with
   `127.5.5.5/8` on `lo` and a route to the LB bridge would fail with
   `-EINVAL` before the packet touches the wire.

2. **ns-external had no link-layer connectivity to hl-br0.** Without a
   veth pair connecting the namespace to the bridge, packets cannot reach
   the macvlan children. The original `ip netns exec ns-external curl ...`
   would return `Network is unreachable`.

## Decision

1. **Carve `10.44.0.0/24` into four /26s for mock country CIDRs.** All
   mock ranges are RFC1918 addresses on the existing test bridge, so the
   kernel has no objection to routing them across interfaces.

   | Range | Code | Purpose |
   |-------|------|---------|
   | `10.44.0.0/26` | US | Bridge itself (`10.44.0.1`), test process |
   | `10.44.0.64/26` | CA | Reserved for future multi-country tests |
   | `10.44.0.128/26` | EU | ns-external veth endpoint |
   | `10.44.0.192/26` | XX | Reserved |

2. **Create a veth pair for ns-external.** The root-ns side is enslaved
   to `hl-br0`; the namespace side gets `10.44.0.129/24` (the `/24`
   netmask gives a connected route for the full bridge subnet, so all
   LB IPs in `10.44.0.10–10.44.0.100` are reachable).

   ```
   Root ns:  veth-ext-us ─── enslaved to hl-br0 ─── macvlan children (LBs)
   ns-external: veth-ext-eu (10.44.0.129/24)
   ```

3. **Replace `127.0.0.0/8` with `10.44.0.128/26` in the mock TSV.** The
   provision script (`e2e-vm/scripts/03-geolite-mock.sh`) writes the new
   carving. The `EU` code on `10.44.0.128/26` matches sources from
   ns-external.

## Consequences

- **Spec B** adds the veth creation + enslavement + IP assignment commands.
  The tear-down remains `ip netns del ns-external` (kernel removes the
  veth peer automatically).
- **Spec D** reuses the same ns-external veth setup for EU-sourced traffic.
- **The mock TSV file** now has four rows instead of two. The `CA` and
  `XX` ranges are available for future test expansion without changing
  the fixture.
- **The `e2e-vm/lima.yaml` provision list** gets an additional entry for
  `scripts/03-geolite-mock.sh` (or the already-existing provision script
  index entry is added — whatever the deployment convention is).
- **No other e2e specs** are affected. The existing harness (one_lb,
  two_lb, churn, oidc_reject) does not reference `127.0.0.0/8` or
  `ns-external`.

## Alternatives considered

### Keep 127.0.0.0/8 and bypass kernel L3 validation
Rejected as fragile. Raw sockets or `iptables -t mangle -j MARK` hacks
to bypass `ip_local_out` would couple the test to kernel internals and
likely break on kernel upgrades.

### Add a separate NS-external subnet (e.g., 192.168.200.0/24)
Workable but would require adding the subnet to the bridge and a route
on the VM host — more provisioning, no benefit over carving the existing
`10.44.0.0/24`.