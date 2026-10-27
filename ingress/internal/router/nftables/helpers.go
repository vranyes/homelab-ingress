package nftables

import (
	"crypto/md5"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

var isoAlpha2RE = regexp.MustCompile(`^[A-Z]{2}$`)

// errTableNotFound lets callers distinguish "daemon has not created its table
// yet" from a real netlink failure.
var errTableNotFound = errors.New("table not found")

func safeName(serviceKey string) string {
	safe := strings.ReplaceAll(serviceKey, "/", "_")
	safe = strings.ReplaceAll(safe, "-", "_")
	if len(safe) > 32 {
		safe = safe[:32]
	}
	hash := md5.Sum([]byte(serviceKey))
	// Prepend first 16 bytes of the hash to ensure uniqueness
	return fmt.Sprintf("%x_%s", hash[:16], safe)
}

func validateService(svc state.LBService) error {
	if len(svc.Targets) == 0 {
		return fmt.Errorf("at least one port target is required")
	}
	for _, cc := range svc.AllowedGeos {
		if !isoAlpha2RE.MatchString(cc) {
			return fmt.Errorf("invalid ISO-3166 alpha-2 code %q", cc)
		}
	}
	for _, raw := range svc.AllowedCIDRs {
		_, ipNet, err := net.ParseCIDR(raw)
		if err != nil {
			return fmt.Errorf("invalid CIDR %q: must be in address/prefix form", raw)
		}
		if ipNet.IP.To4() == nil {
			return fmt.Errorf("invalid CIDR %q: only IPv4 is supported", raw)
		}
	}
	for _, raw := range svc.DeniedCIDRs {
		_, ipNet, err := net.ParseCIDR(raw)
		if err != nil {
			return fmt.Errorf("invalid denied CIDR %q: must be in address/prefix form", raw)
		}
		if ipNet.IP.To4() == nil {
			return fmt.Errorf("invalid denied CIDR %q: only IPv4 is supported", raw)
		}
	}
	switch svc.LBPolicy {
	case "", "round-robin", "hash-source-ip", "random":
	default:
		return fmt.Errorf("invalid lb_policy %q: want round-robin, hash-source-ip or random", svc.LBPolicy)
	}
	if svc.RateLimit != nil {
		if svc.RateLimit.Rate == 0 {
			return fmt.Errorf("rate_limit.rate must be greater than zero")
		}
		if svc.RateLimit.Burst == 0 {
			return fmt.Errorf("rate_limit.burst must be greater than zero")
		}
	}
	for i, target := range svc.Targets {
		if target.Protocol != "tcp" && target.Protocol != "udp" {
			return fmt.Errorf("target %d: unsupported protocol %q (must be tcp or udp)", i, target.Protocol)
		}
		if len(target.Dst) == 0 {
			return fmt.Errorf("target %d: at least one destination is required", i)
		}
		for j, dest := range target.Dst {
			if dest.Address == nil || dest.Address.To4() == nil {
				return fmt.Errorf("target %d destination %d: invalid IPv4 address", i, j)
			}
		}
	}
	return nil
}
