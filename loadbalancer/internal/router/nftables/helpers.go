package nftables

import (
	"fmt"
	"strings"

	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
)

const tableName = "homelab_lb"

// Priority -110: runs before inet fw4's dstnat chain (-100).
const priority = -110

// chainName converts a service key to a valid nftables chain name.
func chainName(prefix, serviceKey string) string {
	safe := strings.ReplaceAll(serviceKey, "/", "_")
	safe = strings.ReplaceAll(safe, "-", "_")
	if len(safe) > 30 {
		safe = safe[:30]
	}
	return prefix + "_" + safe
}

func validateService(svc state.LBService) error {
	if svc.SourceAddress == nil {
		return fmt.Errorf("external IP is required")
	}
	if len(svc.Targets) == 0 {
		return fmt.Errorf("no ports provided")
	}
	return nil
}
