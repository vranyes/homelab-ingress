//go:build linux

package dhcp

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func TestSubnetMaskFromACK_WithMaskOption(t *testing.T) {
	tests := []struct {
		name     string
		mask     net.IPMask
		wantOnes int
		wantBits int
	}{
		{"slash 24", net.CIDRMask(24, 32), 24, 32},
		{"slash 16", net.CIDRMask(16, 32), 16, 32},
		{"slash 28", net.CIDRMask(28, 32), 28, 32},
		{"slash 8", net.CIDRMask(8, 32), 8, 32},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ack, err := dhcpv4.New(dhcpv4.WithOption(dhcpv4.OptSubnetMask(tt.mask)))
			if err != nil {
				t.Fatalf("failed to create DHCP packet: %v", err)
			}

			got := subnetMaskFromACK(ack)

			ones, bits := got.Size()
			if ones != tt.wantOnes || bits != tt.wantBits {
				t.Errorf("subnetMaskFromACK() = /%d (bits=%d), want /%d (bits=%d)",
					ones, bits, tt.wantOnes, tt.wantBits)
			}
		})
	}
}

func TestSubnetMaskFromACK_FallbackWhenMissing(t *testing.T) {
	ack, err := dhcpv4.New()
	if err != nil {
		t.Fatalf("failed to create DHCP packet: %v", err)
	}

	got := subnetMaskFromACK(ack)

	ones, bits := got.Size()
	if ones != 24 || bits != 32 {
		t.Errorf("subnetMaskFromACK() = /%d (bits=%d), want /24 (bits=32)", ones, bits)
	}
}
