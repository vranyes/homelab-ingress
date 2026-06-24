package nftables

import (
	"crypto/md5"
	"fmt"
	"strings"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

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
