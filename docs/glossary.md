# Geoblocking Glossary

| Term | Definition |
|------|-----------|
| **CIDR** | Classless Inter-Domain Routing — a notation for IP address ranges (e.g., `10.44.0.0/24`). |
| **Geo-deny rule** | nftables rule placed at the top of a per-LB chain that drops packets whose source IP is not in the allow-listed country set. The security boundary: drop happens before any DNAT rule. |
| **MaxMind GeoLite2-Country** | A freely-available IP-to-country database. Uses `.mmdb` (MaxMind DB) format. Maps each IPv4/IPv6 prefix to an ISO-3166 alpha-2 country code. |
| **Per-service union set** | In the kernel, one nftables interval set per LB service containing the union of CIDRs from all countries allowed by that service. Contrast with a shared per-country set approach. |
| **ISO-3166 alpha-2** | Two-letter country codes (e.g., `US`, `CA`, `DE`). The format accepted by the `allowed-geos` annotation and validated by the ingress daemon. |
| **Interval set** | nftables set type that supports CIDR/mask elements for efficient membership lookup via radix tree. Used here for `geo_<svc>` sets. |
| **VEPA** | Virtual Ethernet Port Aggregator — a macvlan mode that requires an external switch with reflective-relay capability to forward traffic between child interfaces and the parent. Homelab-ingress uses VEPA mode for macvlan children. |
| **VEPA hairpin relay** | Userspace emulation of an IEEE 802.1Qbg reflective relay, implemented inside the e2e VM via a network namespace + `tc mirred egress mirror`. Allows dnsmasq running in the namespace to serve DHCP to macvlan children on the host bridge. |
| **Mock TSV** | A tab-separated file read by the mock MaxMind reader when `license_key=="mock"`. Format: `<CIDR>\t<ISO-code>`. Used in e2e tests to inject a deterministic IP→country mapping without provisioning a binary MMDB. |
| **ns-external** | A test-only network namespace created inside the e2e VM to source traffic from a non-allow-listed country. Connected to `hl-br0` via a veth pair (`veth-ext-us`/`veth-ext-eu`). |
| **Per-service geo set** | nftables interval set named `geo_<sn>` (sn = safeName(serviceKey)) containing the union of CIDRs for all countries referenced by that service. Created at `AddRules` time, auto-cleaned by `RemoveRules`. |