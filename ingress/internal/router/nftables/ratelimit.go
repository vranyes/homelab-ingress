//go:build linux

package nftables

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

const (
	// rateLimitSetPrefix space-shares the per-service nftables namespace with
	// geo_<sn> and map_<sn>_<port> sets.
	rateLimitSetPrefix = "rl_"
	// rateLimitSetTimeout expires idle client buckets. Without it, every
	// spoofed source address would pin memory forever.
	rateLimitSetTimeout = 10 * time.Minute
	// rateLimitSetSize bounds the number of tracked source addresses per
	// service. At capacity the kernel fails the dynset insert, which takes the
	// non-inverted break path and therefore fails open (traffic is allowed).
	rateLimitSetSize = 1 << 16
)

// rateLimitSetName includes the configured rate and burst so a config change
// materialises a fresh, empty set instead of reusing elements whose per-element
// token buckets were cloned from the old limit expression.
func rateLimitSetName(sn string, rl state.RateLimit) string {
	return fmt.Sprintf("%s%s_%d_%d", rateLimitSetPrefix, sn, rl.Rate, rl.Burst)
}

// ensureRateLimitSet returns the set for this exact rate/burst configuration,
// creating it on first use. It never touches an existing set, so buckets
// survive reconciles that do not change the limit.
func ensureRateLimitSet(ss setStore, table *nftables.Table, sn string, rl state.RateLimit) (*nftables.Set, error) {
	name := rateLimitSetName(sn, rl)
	existing, err := ss.GetSets(table)
	if err != nil {
		return nil, fmt.Errorf("get sets: %w", err)
	}
	for _, s := range existing {
		if s.Name == name {
			return s, nil
		}
	}

	set := &nftables.Set{
		Table:      table,
		Name:       name,
		KeyType:    nftables.TypeIPAddr,
		Dynamic:    true,
		HasTimeout: true,
		Timeout:    rateLimitSetTimeout,
		Size:       rateLimitSetSize,
	}
	if err := ss.AddSet(set, nil); err != nil {
		return nil, fmt.Errorf("AddSet(%s): %w", name, err)
	}
	return set, nil
}

// removeStaleRateLimitSets deletes every rl_<sn>_* set except keep. It is
// called after the per-service chain has been flushed, so the old set is no
// longer referenced. DelSet is only ever issued for sets seen in GetSets:
// deleting a missing set returns ENOENT and aborts the whole netlink batch.
func removeStaleRateLimitSets(ss setStore, table *nftables.Table, sn, keep string) error {
	existing, err := ss.GetSets(table)
	if err != nil {
		return fmt.Errorf("get sets: %w", err)
	}
	prefix := rateLimitSetPrefix + sn + "_"
	for _, s := range existing {
		if s.Name == keep || !strings.HasPrefix(s.Name, prefix) {
			continue
		}
		ss.DelSet(s)
	}
	return nil
}

// buildRateLimitExprs returns:
//
//	ip daddr <extIP> ct state new ip saddr
//	    update @rl_<sn>_<rate>_<burst> { ip saddr limit rate over <rate>/second burst <burst> }
//	    counter drop
//
// The limit expression is cloned into each set element by the kernel, so every
// source IP gets its own token bucket. Over=true inverts the match: under the
// limit the dynset returns NFT_BREAK (the drop is skipped), over the limit
// evaluation continues into the counter and drop verdict.
func buildRateLimitExprs(extIP net.IP, set *nftables.Set, rl state.RateLimit) []expr.Any {
	return []expr.Any{
		// ip daddr <extIP>
		&expr.Payload{
			DestRegister: 0x1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       0x10,
			Len:          0x4,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 0x1,
			Data:     extIP.To4(),
		},

		// ct state new
		&expr.Ct{Key: expr.CtKeySTATE, Register: 0x1},
		&expr.Bitwise{
			SourceRegister: 0x1,
			DestRegister:   0x1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(expr.CtStateBitNEW),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{
			Op:       expr.CmpOpNeq,
			Register: 0x1,
			Data:     binaryutil.NativeEndian.PutUint32(0),
		},

		// ip saddr -> register used as the meter key
		&expr.Payload{
			DestRegister: 0x1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       0x0c,
			Len:          0x4,
		},

		// meter: per-source-IP token bucket
		&expr.Dynset{
			SrcRegKey: 0x1,
			SetName:   set.Name,
			SetID:     set.ID,
			Operation: uint32(unix.NFT_DYNSET_OP_UPDATE),
			Timeout:   rateLimitSetTimeout,
			Invert:    false,
			Exprs: []expr.Any{
				&expr.Limit{
					Type:  expr.LimitTypePkts,
					Rate:  rl.Rate,
					Over:  true,
					Unit:  expr.LimitTimeSecond,
					Burst: rl.Burst,
				},
			},
		},

		// Count only the packets that exceeded the bucket.
		&expr.Counter{},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}
