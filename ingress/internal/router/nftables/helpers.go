package nftables

import (
	"fmt"
	"strings"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

const tableName = "homelab_ingress"

// Priority -110: runs before inet fw4's dstnat chain (-100).
const priority = -110

// ctMarkValue is the conntrack mark applied to ingress traffic.
// The masquerade rule matches only this mark so that only LB-forwarded
// traffic is SNATed.
const ctMarkValue = 0x10

// chainName converts a service key to a valid nftables chain name.
func chainName(prefix, serviceKey string) string {
	safe := strings.ReplaceAll(serviceKey, "/", "_")
	safe = strings.ReplaceAll(safe, "-", "_")
	name := prefix + "_" + safe
	if len(name) > 32 {
		name = name[:32]
	}
	return name
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
