#!/bin/sh
set -ex
apk update
apk add --no-cache \
    nftables \
    iproute2 \
    dnsmasq \
    iptables \
    openrc \
    curl \
    ca-certificates \
    wget \
    tar \
    bash \
    sudo \
    parted \
    util-linux

# Rebuild the CA trust store. Lima's caCerts block injects the host's MITM
# proxy certificate via cloud-init before this script runs; update-ca-certificates
# picks it up so that subsequent curl calls (golang, k3s) succeed over HTTPS.
update-ca-certificates
