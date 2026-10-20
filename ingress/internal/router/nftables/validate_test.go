//go:build linux

package nftables

import (
	"net"
	"testing"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

func validTargets() []state.LBTarget {
	return []state.LBTarget{{
		Protocol: "tcp",
		SrcPort:  80,
		Dst:      []state.LBDestination{{Port: 8080, Address: net.ParseIP("10.0.0.1")}},
	}}
}

func TestValidateService(t *testing.T) {
	t.Run("valid tcp", func(t *testing.T) {
		if err := validateService(state.LBService{ServiceName: "ns/svc", Targets: validTargets()}); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("valid udp", func(t *testing.T) {
		targets := validTargets()
		targets[0].Protocol = "udp"
		if err := validateService(state.LBService{ServiceName: "ns/svc", Targets: targets}); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("no targets rejected", func(t *testing.T) {
		if err := validateService(state.LBService{ServiceName: "ns/svc"}); err == nil {
			t.Fatalf("expected error, got nil")
		}
	})

	t.Run("bad protocol rejected", func(t *testing.T) {
		targets := validTargets()
		targets[0].Protocol = "sctp"
		if err := validateService(state.LBService{ServiceName: "ns/svc", Targets: targets}); err == nil {
			t.Fatalf("expected error, got nil")
		}
	})

	t.Run("no destinations rejected", func(t *testing.T) {
		targets := validTargets()
		targets[0].Dst = nil
		if err := validateService(state.LBService{ServiceName: "ns/svc", Targets: targets}); err == nil {
			t.Fatalf("expected error, got nil")
		}
	})

	t.Run("nil destination address rejected", func(t *testing.T) {
		targets := validTargets()
		targets[0].Dst[0].Address = nil
		if err := validateService(state.LBService{ServiceName: "ns/svc", Targets: targets}); err == nil {
			t.Fatalf("expected error, got nil")
		}
	})

	t.Run("non-IPv4 rejected", func(t *testing.T) {
		targets := validTargets()
		targets[0].Dst[0].Address = net.ParseIP("fd00::1")
		if err := validateService(state.LBService{ServiceName: "ns/svc", Targets: targets}); err == nil {
			t.Fatalf("expected error, got nil")
		}
	})
}

func TestSafeName(t *testing.T) {
	a := safeName("ns/svc")
	b := safeName("ns/svc")
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	// Sanitization must not leave path separators or dashes.
	for _, r := range a {
		if r == '/' || r == '-' {
			t.Fatalf("unsanitized char in %q", a)
		}
	}
	if other := safeName("ns/other"); other == a {
		t.Fatalf("collision between distinct keys: %q", a)
	}
	// Long keys are truncated but stay distinct via the hash prefix:
	// 32 hex chars + "_" + at most 32 sanitized chars = 65.
	const maxLen = 32 + 1 + 32
	long1 := safeName("very-long-namespace-name-that-exceeds-limits/service-one")
	long2 := safeName("very-long-namespace-name-that-exceeds-limits/service-two")
	if len(long1) > maxLen {
		t.Fatalf("unexpectedly long (%d > %d): %q", len(long1), maxLen, long1)
	}
	if long1 == long2 {
		t.Fatalf("truncated names collide: %q", long1)
	}
}
