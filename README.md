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

See [docs/GEOBLOCKING_PLAN.md](docs/GEOBLOCKING_PLAN.md) and
[ADR 0006](docs/adr/0006-dbip-lite-provider.md).

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
