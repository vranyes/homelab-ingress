package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

func testChecker(cfg Config) *Checker {
	cfg.ApplyDefaults()
	c := NewChecker(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c
}

func failDial(_ string, _ string, _ time.Duration) (net.Conn, error) {
	return nil, errors.New("connection refused")
}

func okDial(_ string, _ string, _ time.Duration) (net.Conn, error) {
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}

func tcpTarget(key, addr string, port uint16) Target {
	return Target{ServiceKey: key, Address: addr, Port: port, Protocol: "tcp"}
}

func TestHysteresis(t *testing.T) {
	c := testChecker(Config{FailThreshold: 2, SuccessThreshold: 2})
	key := backendKey{service: "ns/svc", addr: "10.0.0.1", port: 8080}

	if !c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("unknown backend must report healthy (fail-open)")
	}

	// One failure must not flip: FailThreshold is 2.
	if flipped := c.recordResult(key, "tcp", false); flipped {
		t.Fatal("single failure must not flip health")
	}
	if !c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("backend must stay healthy after one failure")
	}

	// Second consecutive failure flips to unhealthy.
	if flipped := c.recordResult(key, "tcp", false); !flipped {
		t.Fatal("second consecutive failure must flip health")
	}
	if c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("backend must be unhealthy after FailThreshold failures")
	}

	// One success must not flip back: SuccessThreshold is 2.
	if flipped := c.recordResult(key, "tcp", true); flipped {
		t.Fatal("single success must not flip health back")
	}
	if c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("backend must stay unhealthy after one success")
	}

	if flipped := c.recordResult(key, "tcp", true); !flipped {
		t.Fatal("second consecutive success must flip health back")
	}
	if !c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("backend must be healthy after SuccessThreshold successes")
	}
}

func TestFailureCounterResetsOnSuccess(t *testing.T) {
	c := testChecker(Config{FailThreshold: 2, SuccessThreshold: 2})
	key := backendKey{service: "ns/svc", addr: "10.0.0.1", port: 8080}

	c.recordResult(key, "tcp", false)
	c.recordResult(key, "tcp", true) // resets the failure streak
	c.recordResult(key, "tcp", false)
	if !c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("interleaved success must reset the failure streak")
	}
}

func TestCheckOnceUDPPassthrough(t *testing.T) {
	c := testChecker(Config{})
	c.dial = failDial // even a dial that always fails must not affect UDP

	targets := []Target{
		{ServiceKey: "ns/svc", Address: "10.0.0.9", Port: 5353, Protocol: "udp"},
	}
	if changed := c.checkOnce(targets); changed {
		t.Fatal("UDP target must never flip health")
	}
	if !c.IsHealthy("ns/svc", "10.0.0.9", 5353) {
		t.Fatal("UDP target must always report healthy")
	}
	if got := len(c.Stats()); got != 0 {
		t.Fatalf("UDP target must not be tracked, Stats has %d entries", got)
	}
}

func TestCheckOnceTCPDownAfterThreshold(t *testing.T) {
	c := testChecker(Config{FailThreshold: 2, SuccessThreshold: 2})
	c.dial = failDial
	targets := []Target{tcpTarget("ns/svc", "10.0.0.1", 8080)}

	c.checkOnce(targets)
	if !c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("one failed round must not mark the backend down")
	}
	if changed := c.checkOnce(targets); !changed {
		t.Fatal("second failed round must report a flip")
	}
	if c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("backend must be unhealthy after FailThreshold failed rounds")
	}

	stats := c.Stats()
	if len(stats) != 1 {
		t.Fatalf("Stats must track the probed backend, got %d entries", len(stats))
	}
	if stats[0].Healthy || stats[0].ProbesTotal != 2 || stats[0].ProbesFailed != 2 {
		t.Fatalf("unexpected stats: %+v", stats[0])
	}
}

func TestCheckOnceRecovery(t *testing.T) {
	c := testChecker(Config{FailThreshold: 1, SuccessThreshold: 1})
	c.dial = failDial
	targets := []Target{tcpTarget("ns/svc", "10.0.0.1", 8080)}
	c.checkOnce(targets)
	if c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("backend must be down after failed probe")
	}
	c.dial = okDial
	if changed := c.checkOnce(targets); !changed {
		t.Fatal("successful probe must flip the backend back")
	}
	if !c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("backend must be healthy after successful probe")
	}
}

func TestPruneRemovesStaleBackends(t *testing.T) {
	c := testChecker(Config{FailThreshold: 1})
	c.dial = failDial
	targets := []Target{tcpTarget("ns/svc", "10.0.0.1", 8080)}
	c.checkOnce(targets)
	if len(c.Stats()) != 1 {
		t.Fatal("backend must be tracked after probing")
	}
	c.checkOnce(nil)
	if len(c.Stats()) != 0 {
		t.Fatal("backends absent from State must be pruned")
	}
	if !c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("pruned backend must report healthy again (fail-open)")
	}
}

func fluentState() *state.State {
	st := state.NewState("eth0", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	st.Services["ns/svc"] = &state.LBService{
		ServiceName:   "ns/svc",
		SourceAddress: net.ParseIP("192.168.1.10"),
		Targets: []state.LBTarget{
			{
				Protocol: "tcp",
				SrcPort:  80,
				Dst:      []state.LBDestination{{Port: 8080, Address: net.ParseIP("10.0.0.1")}},
			},
			{
				Protocol: "udp",
				SrcPort:  53,
				Dst:      []state.LBDestination{{Port: 5353, Address: net.ParseIP("10.0.0.2")}},
			},
		},
	}
	return st
}

func TestTargetsFromSnapshot(t *testing.T) {
	targets := targetsFromSnapshot(fluentState().Clone())
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(targets))
	}
	seen := map[string]string{}
	for _, tg := range targets {
		seen[tg.Address] = tg.Protocol
	}
	if seen["10.0.0.1"] != "tcp" || seen["10.0.0.2"] != "udp" {
		t.Fatalf("unexpected targets: %v", seen)
	}
}

func TestRunFlipsAndNotifiesThenCancels(t *testing.T) {
	c := testChecker(Config{Interval: 10 * time.Millisecond, FailThreshold: 1, SuccessThreshold: 1})
	c.dial = failDial
	st := fluentState()

	changed := make(chan struct{}, 1)
	c.OnChange = func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, st)
	}()

	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("OnChange not called after backend went down")
	}
	if c.IsHealthy("ns/svc", "10.0.0.1", 8080) {
		t.Fatal("TCP backend must be unhealthy after failed probes")
	}
	if !c.IsHealthy("ns/svc", "10.0.0.2", 5353) {
		t.Fatal("UDP backend must stay healthy")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}
}

func TestProbeRealDialRefused(t *testing.T) {
	// 127.0.0.1 with a closed port: fast, deterministic, no network needed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind loopback for probe test")
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()

	c := testChecker(Config{})
	if c.Probe("127.0.0.1", port) {
		t.Fatal("probe of a closed loopback port must fail")
	}
}
