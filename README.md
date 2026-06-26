# Homelab Ingress

Alright, so here's the deal, this isn't a very practical project.
Basically its NLB for your homelab but w/o multi AZ (duh).

## Features

- Secure by default with auto-generated self-signed certificate.
- OIDC backed authorization.
- Programmatic provisioning of macvlan interfaces on top of a specified base interface.
- DHCP based IPv4 address acquisition and management, incl.
  lease renewal.
- Round-robin style loadbalancing to specified backend destinations.
- A Kubernetes controller implementing a homelab-ingress LoadBalancer class.
- A cli for interacting with the ingress service.
- Easy to manage masquerade rules for "on-prem" traffic.

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
a single client as long at the requesting MAC address is different.
All I had to do was write a simple service that loadbalanced L4 traffic to downstream
destinations, kinda like AWS NLB.
