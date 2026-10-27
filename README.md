# Homelab Ingress

Alright, so here's the deal, this isn't a very practical project.
Basically it's an NLB for your homelab but w/o multi AZ (duh).

## Features

- Secure by default with auto-generated self-signed certificate.
- OIDC backed authorization.
- Programmatic provisioning of macvlan interfaces on top of a specified base interface.
- DHCP based IPv4 address acquisition and management, incl.
  lease renewal.
- Round-robin style load balancing to specified backend destinations.
- A Kubernetes controller implementing a homelab-ingress LoadBalancer class.
- Country-based access control per LoadBalancer Service (MaxMind GeoLite2 or
  free DB-IP Lite; DB-IP needs no account or key).
- Per-client new-connection rate limiting per LoadBalancer Service.
- A cli for interacting with the ingress service.
- Easy to manage masquerade rules for "on-prem" traffic.

## Geo-blocking

Annotate a `LoadBalancer` Service with comma-separated ISO-3166 alpha-2 codes:

```yaml
metadata:
  annotations:
    homelab-ingress.vranyes.com/allowed-geos: "US,CA"
```

Traffic from other countries is dropped at prerouting, before DNAT. Enable the
feature in the ingress config with either provider:

```yaml
geoip:
  provider: dbip            # free, no key; or "maxmind" with a license_key
```

### LAN bypass (`allowed-cidrs`)

Private IPs have no country in the geo database, so a geo-blocked Service
locks out LAN clients too. Add IPv4 CIDRs that are always allowed alongside
the country list:

```yaml
metadata:
  annotations:
    homelab-ingress.vranyes.com/allowed-geos: "US,CA"
    homelab-ingress.vranyes.com/allowed-cidrs: "192.168.1.0/24, 10.0.0.0/8"
```

Either annotation (or both) switches the Service to deny-by-default with a
union set: a packet is accepted when its source is in any allowed country
**or** any listed CIDR. Any IPv4 CIDR works (RFC 1918, CGNAT `100.64.0.0/10`,
link-local, ...); IPv6 is rejected because the nftables table is IPv4-only.
Static CIDRs need no geo database, so a CIDR-only Service works with
`geoip.provider` unset. An invalid CIDR keeps the last-good CIDRs (none on
first install) and still reconciles — it never silently opens the Service.

See [docs/GEOBLOCKING_PLAN.md](docs/GEOBLOCKING_PLAN.md) and
[ADR 0006](docs/adr/0006-dbip-lite-provider.md).

## Rate limiting

Cap new connections from a single source IP on a `LoadBalancer` Service:

```yaml
metadata:
  annotations:
    homelab-ingress.vranyes.com/rate-limit: "5"        # new connections/sec per client IP
    homelab-ingress.vranyes.com/rate-limit-burst: "10" # optional; defaults to the rate
```

Only new connections are counted (TCP handshakes and the first packet of a UDP
flow); established connections are never throttled. Excess packets are dropped
in prerouting and counted per service. The bucket is shared across all ports of
the Service, and is applied after geo-blocking and before DNAT. A change to
either annotation takes effect live and starts a fresh bucket; removing the
`rate-limit` annotation removes the limit.

See [ADR 0007](docs/adr/0007-per-client-connection-rate-limiting.md).

## OpenWrt

The router package installs a procd init script plus a UCI config that is
translated into the YAML the daemon reads, including the geo-blocking options.
See [docs/openwrt.md](docs/openwrt.md) for the option reference and the
state/cache layout.

## Requirements

- A Linux based router with nftables.
- An internet connection.
- An ISP who will give you multiple public IPv4 addresses.
- Backends the router can route to.

## Backstory

Basically I was sick of using Tailscale DNS names and wanted to expose services
to the public internet 😳 under my own domain and also expose services using stuff
like Gateway API.

I was inspired by the fact that my ISP will delegate multiple IPv4 addresses to
a single client as long as the requesting MAC address is different.
All I had to do was write a simple service that loadbalanced L4 traffic to downstream
destinations, kinda like AWS NLB.
