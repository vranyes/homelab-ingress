//go:build linux

package harness

import (
	"crypto/md5"
	"fmt"
	"strings"

	"github.com/google/nftables"
)

// nftTableName mirrors the ingress daemon's table name: "homelab_ingress"
// (see ingress/internal/router/nftables/const.go).
const nftTableName = "homelab_ingress"

// nftPerLBPrefix mirrors the per-LB chain prefix used by the ingress daemon:
// "pre_" + safeName(serviceKey) (see nftables/manager.go AddRules).
const nftPerLBPrefix = "pre_"

// LBChains returns the names of per-LB nftables chains in the homelab_ingress
// table. A per-LB chain is one whose name starts with nftPerLBPrefix ("pre_").
func LBChains() ([]string, error) {
	conn, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("open nftables: %w", err)
	}
	chains, err := conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, fmt.Errorf("list chains: %w", err)
	}
	var out []string
	for _, c := range chains {
		if c.Table != nil && c.Table.Name == nftTableName && strings.HasPrefix(c.Name, nftPerLBPrefix) {
			out = append(out, c.Name)
		}
	}
	return out, nil
}

// AssertNoLBChains returns nil iff there are zero per-LB nftables chains.
func AssertNoLBChains() error {
	chains, err := LBChains()
	if err != nil {
		return err
	}
	if len(chains) != 0 {
		return fmt.Errorf("expected 0 lb chains, got %d: %v", len(chains), chains)
	}
	return nil
}

// AssertLBChainExists returns nil iff a chain named like the per-LB chain for
// `serviceKey` exists in nftables. The chain name is computed by mirroring
// nftables.safeName (md5-prefixed sanitized service key). The function is
// tolerant — empty result and a transient nftables failure both return nil;
// only a confirmed wrong chain count returns an error.
func AssertLBChainExists(serviceKey string) error {
	expected := nftPerLBPrefix + safeNameNFT(serviceKey)
	chains, err := LBChains()
	if err != nil {
		return err
	}
	for _, c := range chains {
		if c == expected {
			return nil
		}
	}
	return fmt.Errorf("expected lb chain %q for service %q, found %v", expected, serviceKey, chains)
}

// safeNameNFT mirrors ingress/internal/router/nftables.safeName so the e2e
// suite can independently compute the expected per-LB chain name.
func safeNameNFT(serviceKey string) string {
	safe := strings.ReplaceAll(serviceKey, "/", "_")
	safe = strings.ReplaceAll(safe, "-", "_")
	if len(safe) > 32 {
		safe = safe[:32]
	}
	hash := md5.Sum([]byte(serviceKey))
	return fmt.Sprintf("%x_%s", hash[:16], safe)
}
