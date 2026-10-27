// This file is intentionally portable (no linux build tag) so the health
// filtering logic is unit-testable on any platform. The nftables rule
// builders that consume it stay linux-only.

package nftables

import (
	"log/slog"
	"strings"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

// filterHealthyTargets returns the destinations of target that the health
// checker currently considers healthy.
//
// Only TCP destinations are filtered: UDP has no handshake, so the checker
// never probes it and every UDP destination passes through untouched.
// When hc is nil, filtering is disabled and the full set is returned.
//
// Fail-open: if filtering would remove every destination, the original set
// is returned and a health_all_down warning is logged, so a checker outage
// (or a short correlated backend outage) degrades to "try everything"
// instead of black-holing the service. The input target is never mutated;
// the returned slice is either target.Dst or a fresh slice.
func filterHealthyTargets(serviceKey string, target state.LBTarget, hc HealthChecker, logger *slog.Logger) []state.LBDestination {
	if hc == nil || !strings.EqualFold(target.Protocol, "tcp") {
		return target.Dst
	}
	kept := make([]state.LBDestination, 0, len(target.Dst))
	for _, d := range target.Dst {
		addr := ""
		if d.Address != nil {
			addr = d.Address.String()
		}
		if hc.IsHealthy(serviceKey, addr, d.Port) {
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		logger.Warn("health_all_down: all backends unhealthy, failing open",
			"service", serviceKey,
			"port", target.SrcPort,
		)
		return target.Dst
	}
	if len(kept) < len(target.Dst) {
		logger.Info("health filtered unhealthy backends",
			"service", serviceKey,
			"port", target.SrcPort,
			"healthy", len(kept),
			"total", len(target.Dst),
		)
	}
	return kept
}
