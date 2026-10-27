//go:build linux

package dhcp

import (
	"net"
	"testing"
	"time"
)

// TestLeaseExpiryUnix pins the metrics derivation: a lease whose ACK carries
// a lease time yields an expiry ~leaseTime in the future; nil, empty and
// corrupt inputs yield 0 (unknown) instead of panicking.
func TestLeaseExpiryUnix(t *testing.T) {
	if got := leaseExpiryUnix(nil); got != 0 {
		t.Fatalf("nil info = %d, want 0", got)
	}
	if got := leaseExpiryUnix(&LeaseInfo{}); got != 0 {
		t.Fatalf("empty info = %d, want 0", got)
	}
	if got := leaseExpiryUnix(&LeaseInfo{ACKBytes: []byte{0x01, 0x02}}); got != 0 {
		t.Fatalf("corrupt ACKBytes = %d, want 0", got)
	}

	ip := net.ParseIP("10.44.0.55").To4()
	mask := net.CIDRMask(24, 32)
	ack := syntheticACK(t, ip, mask, net.ParseIP("10.44.0.2").To4(), 24*time.Hour)
	info := &LeaseInfo{ACKBytes: ack.ToBytes()}

	before := time.Now().Unix()
	got := leaseExpiryUnix(info)
	wantLow, wantHigh := before+23*time.Hour.Milliseconds()/1000, before+25*time.Hour.Milliseconds()/1000
	if got < wantLow || got > wantHigh {
		t.Fatalf("expiry = %d, want within [%d, %d] (now + 24h)", got, wantLow, wantHigh)
	}
}
