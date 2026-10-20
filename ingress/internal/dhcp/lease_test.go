//go:build linux

package dhcp

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
)

// syntheticACK builds an ACK packet with the given parameters without
// touching the network. Discovery is built for "lo" (always present on
// Linux, no privileges needed — only packet construction, no sockets).
func syntheticACK(t *testing.T, yourIP net.IP, mask net.IPMask, serverIP net.IP, leaseTime time.Duration) *dhcpv4.DHCPv4 {
	t.Helper()
	req, err := dhcpv4.NewDiscoveryForInterface("lo")
	if err != nil {
		t.Fatalf("discovery for lo: %v", err)
	}
	ack, err := dhcpv4.NewReplyFromRequest(req,
		dhcpv4.WithYourIP(yourIP),
		dhcpv4.WithOption(dhcpv4.OptSubnetMask(mask)),
		dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(leaseTime)),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(serverIP)),
	)
	if err != nil {
		t.Fatalf("reply from request: %v", err)
	}
	return ack
}

// TestLeaseRoundTripPreservesIPMaskOptionsAndServiceName pins the persist/
// restore codec: everything the Manager needs after a restart (address,
// mask, ACK bytes carrying all options, service identity) must survive
// ToBytes -> newLeaseInfo -> leaseFromInfo -> FromBytes.
func TestLeaseRoundTripPreservesIPMaskOptionsAndServiceName(t *testing.T) {
	wantIP := net.ParseIP("10.44.0.55").To4()
	wantMask := net.IPMask{255, 255, 255, 0}
	wantServer := net.ParseIP("10.44.0.2").To4()

	ack := syntheticACK(t, wantIP, wantMask, wantServer, 24*time.Hour)
	lease := &nclient4.Lease{ACK: ack, CreationTime: time.Now()}

	info := newLeaseInfo(lease, wantIP, wantMask)
	if info == nil {
		t.Fatalf("newLeaseInfo returned nil for a valid lease")
	}
	if !info.Address.Equal(wantIP) {
		t.Fatalf("Address = %s, want %s", info.Address, wantIP)
	}
	if info.Mask.String() != wantMask.String() {
		t.Fatalf("Mask = %s, want %s", info.Mask, wantMask)
	}
	if !bytes.Equal(info.ACKBytes, ack.ToBytes()) {
		t.Fatalf("ACKBytes do not match the original packet bytes")
	}

	info.ServiceName = "ns/svc"

	rt := leaseFromInfo(info)
	if rt == nil {
		t.Fatalf("leaseFromInfo returned nil for valid info")
	}
	if rt.Offer != nil {
		t.Fatalf("expected nil Offer (none persisted), got %+v", rt.Offer)
	}
	got := rt.ACK
	if !got.YourIPAddr.Equal(wantIP) {
		t.Fatalf("YourIPAddr = %s, want %s", got.YourIPAddr, wantIP)
	}
	for _, code := range []dhcpv4.OptionCode{
		dhcpv4.OptionSubnetMask,
		dhcpv4.OptionIPAddressLeaseTime,
		dhcpv4.OptionServerIdentifier,
	} {
		if !bytes.Equal(got.Options.Get(code), ack.Options.Get(code)) {
			t.Fatalf("option %v changed across round-trip", code)
		}
	}
	if got.Options.Get(dhcpv4.OptionSubnetMask) == nil {
		t.Fatalf("subnet mask option missing after round-trip")
	}
}

// TestLeaseNilAndCorruptInputs pins the rejection paths: nil lease, nil
// ACK, empty bytes, and undecodable bytes must all yield nil, never panic.
func TestLeaseNilAndCorruptInputs(t *testing.T) {
	if newLeaseInfo(nil, nil, nil) != nil {
		t.Fatalf("newLeaseInfo(nil) must return nil")
	}
	if newLeaseInfo(&nclient4.Lease{}, nil, nil) != nil {
		t.Fatalf("newLeaseInfo with nil ACK must return nil")
	}
	if leaseFromInfo(nil) != nil {
		t.Fatalf("leaseFromInfo(nil) must return nil")
	}
	if leaseFromInfo(&LeaseInfo{}) != nil {
		t.Fatalf("leaseFromInfo with empty ACKBytes must return nil")
	}
	if leaseFromInfo(&LeaseInfo{ACKBytes: []byte{0x01, 0x02, 0x03}}) != nil {
		t.Fatalf("leaseFromInfo with corrupt ACKBytes must return nil")
	}
}
