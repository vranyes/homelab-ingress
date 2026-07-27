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

# --- VEPA hairpin relay prerequisites ---------------------------------------
# The e2e topology mirrors the production VEPA macvlan data plane inside a
# single lima VM. dnsmasq and a veth peer live in a dedicated `ns-server`
# network namespace; a `tc mirred egress` rule on the namespace-side veth
# acts as the reflective relay (https://wiki.linuxfoundation.org/networking/vepa)
# that VEPA mode requires. The Linux macvlan driver would otherwise drop
# frames whose source MAC bounces back into the same host stack.
#
# Both act_mirred and macvlan are built into Alpine's linux-virt kernel
# (CONFIG_NET_ACT_MIRRED=y, CONFIG_MACVLAN=y). Probe them explicitly so that
# a future base image change fails the bootstrap with an actionable error
# instead of a mysterious "DHCP never gets a lease" timeout mid-test.
for mod in act_mirred macvlan; do
    if ! modprobe -n "$mod" >/dev/null 2>&1 && \
       ! { [ -e "/sys/module/$mod" ] || zgrep -q "^$mod " /proc/modules 2>/dev/null; }; then
        # Try loading anyway — some kernels report the module as unavailable
        # via modprobe -n but accept insmod from the built-in set.
        if ! modprobe "$mod" 2>/dev/null; then
            echo "ERROR: kernel module '$mod' is neither built-in nor loadable." >&2
            echo "       VEPA relay (e2e-vm/lima.yaml) cannot function without it." >&2
            exit 1
        fi
    fi
done

# Sanity-check the mirred action end-to-end: spin up a transient netns with a
# veth pair and confirm `tc filter add ... mirred` is accepted by the running
# kernel. Discards the test namespace on the way out.
{
    ip netns add _mirred_probe
    ip link add _probe_a type veth peer name _probe_b
    ip link set _probe_b netns _mirred_probe
    ip link set _probe_a up
    ip -n _mirred_probe link set _probe_b up
    ip -n _mirred_probe link set lo up

    tc -n _mirred_probe qdisc add dev _probe_b ingress
    if ! tc -n _mirred_probe filter add dev _probe_b ingress \
            protocol all pref 1 u32 match u8 0 0 \
            action mirred egress mirror dev _probe_b; then
        echo "ERROR: kernel does not accept tc mirred on the virt image." >&2
        echo "       Ensure CONFIG_NET_ACT_MIRRED=y in the running kernel." >&2
        ip link del _probe_a 2>/dev/null || true
        ip netns del _mirred_probe 2>/dev/null || true
        exit 1
    fi

    ip link del _probe_a
    ip netns del _mirred_probe
}
