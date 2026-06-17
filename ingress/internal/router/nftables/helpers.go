package nftables

import (
	"encoding/binary"
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

// ChainName converts a service key to a valid nftables chain name.
func ChainName(prefix, serviceKey string) string {
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

// DestinationsEqual checks whether raw set element values match the desired
// destinations, ignoring the kernel's element ordering.
func DestinationsEqual(values [][]byte, dests []state.LBDestination) bool {
	if len(values) != len(dests) {
		return false
	}
	expected := make(map[string]struct{}, len(dests))
	for _, d := range dests {
		if len(d.Address) != 4 {
			return false
		}
		val := make([]byte, 8)
		copy(val[0:4], d.Address)
		binary.BigEndian.PutUint16(val[4:6], uint16(d.Port))
		expected[string(val)] = struct{}{}
	}
	for _, v := range values {
		if _, ok := expected[string(v)]; !ok {
			return false
		}
	}
	return true
}
