// Package health implements active TCP health-checking of load-balancer
// backends.
//
// The controller already filters endpoint slices to Ready endpoints, but that
// only covers what the API server reports. This checker is a second layer for
// out-of-band failures: a stale endpoint IP, a firewall change, or a crashed
// pod whose slice entry has not been removed yet. Every Interval the checker
// snapshots State, TCP-dials each backend, and records per-backend health
// with flap hysteresis (FailThreshold consecutive failures to mark down,
// SuccessThreshold consecutive successes to mark up again).
//
// UDP targets are never probed and always report healthy: UDP has no
// handshake, so a dial cannot distinguish "listening" from "black hole".
// Unknown backends (never probed) also report healthy, so rule planning
// fails open until the first probe completes.
package health

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/vranyes/homelab-ingress/ingress/internal/metrics"
	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

const (
	// DefaultInterval is how often all current backends are probed.
	DefaultInterval = 10 * time.Second
	// DefaultTimeout bounds a single TCP probe.
	DefaultTimeout = 2 * time.Second
	// DefaultFailThreshold is the consecutive failures before a backend is
	// marked unhealthy.
	DefaultFailThreshold = 2
	// DefaultSuccessThreshold is the consecutive successes before an
	// unhealthy backend is marked healthy again.
	DefaultSuccessThreshold = 2
)

// Config tunes the Checker. Zero values select the defaults (see
// ApplyDefaults).
type Config struct {
	Interval         time.Duration
	Timeout          time.Duration
	FailThreshold    int
	SuccessThreshold int
}

// ApplyDefaults fills unset (non-positive) fields with the defaults.
func (c *Config) ApplyDefaults() {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.FailThreshold <= 0 {
		c.FailThreshold = DefaultFailThreshold
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = DefaultSuccessThreshold
	}
}

// Target is a single probeable backend.
type Target struct {
	ServiceKey string
	Address    string
	Port       uint16
	Protocol   string
}

// BackendStats is a point-in-time snapshot of one tracked backend. It is the
// seam a future metrics package can scrape; until one exists, operators rely
// on the transition logs emitted by the Checker.
type BackendStats struct {
	ServiceKey           string
	Address              string
	Port                 uint16
	Protocol             string
	Healthy              bool
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	ProbesTotal          uint64
	ProbesFailed         uint64
}

type backendKey struct {
	service string
	addr    string
	port    uint16
}

func (k backendKey) String() string {
	return fmt.Sprintf("%s/%s:%d", k.service, k.addr, k.port)
}

type backend struct {
	protocol           string
	healthy            bool
	consecutiveFailed  int
	consecutiveSuccess int
	probesTotal        uint64
	probesFailed       uint64
}

// Checker tracks backend health. It is safe for concurrent use.
type Checker struct {
	cfg    Config
	logger *slog.Logger

	mu       sync.RWMutex
	backends map[backendKey]*backend

	dial func(network, address string, timeout time.Duration) (net.Conn, error)

	// OnChange, when non-nil, is invoked from the Run loop (not holding any
	// Checker lock) whenever at least one backend flips health state.
	// Wiring it to a rule reconciler keeps nftables maps converging on
	// probe results between state events.
	OnChange func()
}

// NewChecker returns a Checker using cfg (defaults applied). A nil logger
// discards logs.
func NewChecker(cfg Config, logger *slog.Logger) *Checker {
	cfg.ApplyDefaults()
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Checker{
		cfg:      cfg,
		logger:   logger,
		backends: make(map[backendKey]*backend),
		dial: func(network, address string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout(network, address, timeout)
		},
	}
}

// Probe dials addr:port over TCP with the configured timeout and reports
// whether the connection succeeded.
func (c *Checker) Probe(addr string, port uint16) bool {
	conn, err := c.dial("tcp", net.JoinHostPort(addr, strconv.Itoa(int(port))), c.cfg.Timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// IsHealthy reports whether a backend is currently considered healthy.
// Backends never probed (including UDP targets, which are never probed)
// report healthy: callers fail open.
func (c *Checker) IsHealthy(serviceKey, addr string, port uint16) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, ok := c.backends[backendKey{service: serviceKey, addr: addr, port: port}]
	if !ok {
		return true
	}
	return b.healthy
}

// Stats returns a snapshot of every tracked backend for metrics/logging.
func (c *Checker) Stats() []BackendStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]BackendStats, 0, len(c.backends))
	for k, b := range c.backends {
		out = append(out, BackendStats{
			ServiceKey:           k.service,
			Address:              k.addr,
			Port:                 k.port,
			Protocol:             b.protocol,
			Healthy:              b.healthy,
			ConsecutiveFailures:  b.consecutiveFailed,
			ConsecutiveSuccesses: b.consecutiveSuccess,
			ProbesTotal:          b.probesTotal,
			ProbesFailed:         b.probesFailed,
		})
	}
	return out
}

// recordResult folds one probe outcome into the backend state and reports
// whether the healthy flag flipped. Every outcome also feeds the
// homelab_ingress_backend_up / homelab_ingress_backend_probes_total series.
func (c *Checker) recordResult(key backendKey, protocol string, ok bool) (flipped bool) {
	c.mu.Lock()
	b, found := c.backends[key]
	if !found {
		b = &backend{protocol: protocol, healthy: true}
		c.backends[key] = b
	}
	b.probesTotal++
	if ok {
		b.consecutiveFailed = 0
		b.consecutiveSuccess++
		if !b.healthy && b.consecutiveSuccess >= c.cfg.SuccessThreshold {
			b.healthy = true
			flipped = true
		}
	} else {
		b.probesFailed++
		b.consecutiveSuccess = 0
		b.consecutiveFailed++
		if b.healthy && b.consecutiveFailed >= c.cfg.FailThreshold {
			b.healthy = false
			flipped = true
		}
	}
	healthy := b.healthy
	c.mu.Unlock()

	metrics.IncBackendProbe(key.service, ok)
	metrics.SetBackendUp(key.service, key.addr, key.port, healthy)
	return flipped
}

// checkOnce probes every TCP target and records the results. It returns true
// when at least one backend flipped health state. Non-TCP targets are
// skipped: UDP has no handshake, so a dial result is meaningless.
func (c *Checker) checkOnce(targets []Target) bool {
	type result struct {
		key      backendKey
		protocol string
		ok       bool
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		if t.Protocol != "" && t.Protocol != "tcp" {
			results[i].ok = true
			results[i].key = backendKey{}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = result{
				key:      backendKey{service: t.ServiceKey, addr: t.Address, port: t.Port},
				protocol: t.Protocol,
				ok:       c.Probe(t.Address, t.Port),
			}
		}()
	}
	wg.Wait()

	changed := false
	for _, r := range results {
		if r.key == (backendKey{}) {
			continue
		}
		if c.recordResult(r.key, r.protocol, r.ok) {
			changed = true
			c.logger.Warn("backend health flipped",
				"backend", r.key.String(),
				"healthy", c.IsHealthy(r.key.service, r.key.addr, r.key.port),
			)
		}
	}
	c.prune(targets)
	return changed
}

// prune drops backends that no longer exist in State so the map cannot grow
// across service churn. Callers must not hold c.mu (checkOnce does not).
func (c *Checker) prune(targets []Target) {
	live := make(map[backendKey]struct{}, len(targets))
	for _, t := range targets {
		live[backendKey{service: t.ServiceKey, addr: t.Address, port: t.Port}] = struct{}{}
	}
	c.mu.Lock()
	var pruned []backendKey
	for k := range c.backends {
		if _, ok := live[k]; !ok {
			delete(c.backends, k)
			pruned = append(pruned, k)
		}
	}
	c.mu.Unlock()
	for _, k := range pruned {
		metrics.DeleteBackendUp(k.service, k.addr, k.port)
	}
}

// targetsFromSnapshot flattens every destination of every service into probe
// targets.
func targetsFromSnapshot(snap *state.State) []Target {
	var out []Target
	for key, svc := range snap.Services {
		if svc == nil {
			continue
		}
		for _, t := range svc.Targets {
			for _, d := range t.Dst {
				if d.Address == nil {
					continue
				}
				out = append(out, Target{
					ServiceKey: key,
					Address:    d.Address.String(),
					Port:       d.Port,
					Protocol:   t.Protocol,
				})
			}
		}
	}
	return out
}

// Run probes all current backends every Interval until ctx is done. The
// snapshot is read-only (State.Clone); persisted state is never mutated.
// An initial pass runs immediately so rule planning converges without
// waiting a full interval after startup.
func (c *Checker) Run(ctx context.Context, st *state.State) {
	if c.checkSnapshot(st) && c.OnChange != nil {
		c.OnChange()
	}
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.checkSnapshot(st) && c.OnChange != nil {
				c.OnChange()
			}
		}
	}
}

func (c *Checker) checkSnapshot(st *state.State) bool {
	snap := st.Clone()
	return c.checkOnce(targetsFromSnapshot(snap))
}
