// Portable (no linux build tag): exercises filterHealthyTargets without a
// netlink socket, on any platform.

package nftables

import (
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

func healthTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeHealthChecker is a HealthChecker backed by an explicit down-set.
type fakeHealthChecker struct {
	down map[string]bool
}

func (f *fakeHealthChecker) IsHealthy(serviceKey, addr string, port uint16) bool {
	return !f.down[serviceKey+"/"+addr+":"+strconv.Itoa(int(port))]
}

func healthTestTargets() state.LBTarget {
	return state.LBTarget{
		Protocol: "tcp",
		SrcPort:  80,
		Dst: []state.LBDestination{
			{Port: 8080, Address: net.ParseIP("10.0.0.1")},
			{Port: 8080, Address: net.ParseIP("10.0.0.2")},
			{Port: 8080, Address: net.ParseIP("10.0.0.3")},
		},
	}
}

func TestFilterHealthyTargets(t *testing.T) {
	t.Run("nil checker passes everything through", func(t *testing.T) {
		tgt := healthTestTargets()
		got := filterHealthyTargets("ns/svc", tgt, nil, healthTestLogger())
		if len(got) != 3 {
			t.Fatalf("expected 3 destinations, got %d", len(got))
		}
	})

	t.Run("unhealthy TCP backend is excluded", func(t *testing.T) {
		tgt := healthTestTargets()
		hc := &fakeHealthChecker{down: map[string]bool{"ns/svc/10.0.0.2:8080": true}}
		got := filterHealthyTargets("ns/svc", tgt, hc, healthTestLogger())
		if len(got) != 2 {
			t.Fatalf("expected 2 destinations, got %d", len(got))
		}
		for _, d := range got {
			if d.Address.String() == "10.0.0.2" {
				t.Fatal("unhealthy backend must be excluded")
			}
		}
	})

	t.Run("all down fails open to the full set", func(t *testing.T) {
		tgt := healthTestTargets()
		hc := &fakeHealthChecker{down: map[string]bool{
			"ns/svc/10.0.0.1:8080": true,
			"ns/svc/10.0.0.2:8080": true,
			"ns/svc/10.0.0.3:8080": true,
		}}
		got := filterHealthyTargets("ns/svc", tgt, hc, healthTestLogger())
		if len(got) != 3 {
			t.Fatalf("fail-open must keep all 3 destinations, got %d", len(got))
		}
	})

	t.Run("UDP targets always pass through", func(t *testing.T) {
		tgt := healthTestTargets()
		tgt.Protocol = "udp"
		hc := &fakeHealthChecker{down: map[string]bool{
			"ns/svc/10.0.0.1:8080": true,
			"ns/svc/10.0.0.2:8080": true,
			"ns/svc/10.0.0.3:8080": true,
		}}
		got := filterHealthyTargets("ns/svc", tgt, hc, healthTestLogger())
		if len(got) != 3 {
			t.Fatalf("UDP must pass through unfiltered, got %d", len(got))
		}
	})

	t.Run("input target is not mutated", func(t *testing.T) {
		tgt := healthTestTargets()
		hc := &fakeHealthChecker{down: map[string]bool{"ns/svc/10.0.0.1:8080": true}}
		_ = filterHealthyTargets("ns/svc", tgt, hc, healthTestLogger())
		if len(tgt.Dst) != 3 {
			t.Fatalf("filtering must not mutate the service target, got %d", len(tgt.Dst))
		}
		if tgt.Dst[0].Address.String() != "10.0.0.1" {
			t.Fatalf("filtering must not reorder destinations: %v", tgt.Dst[0].Address)
		}
	})
}
