//go:build linux

package nftables

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// buildRoundRobinExprs returns the []expr.Any for a single prerouting rule:
//
//	ip daddr <extIP> <protocol> dport <port> dnat to numgen inc mod <n> map {
//	    0 : <ip0> . <port0>,
//	    1 : <ip1> . <port1>,
//	    ...
//	}
//
// Calls c.AddSet internally (which populates lbMap.ID before the Lookup expr
// references it). Both the set and the rule are queued in the same batch;
// the caller flushes once after all rules are added.
//
// dst keys are destination IPs, values are destination ports.
// Entries are sorted by IP string so the numgen index assignment is stable
// across calls.
func buildRoundRobinExprs(
	c *nftables.Conn,
	sname string,
	table *nftables.Table,
	extIP net.IP,
	protocol string,
	port uint16,
	dst map[string]uint16,
) ([]expr.Any, error) {

	if len(dst) == 0 {
		return nil, fmt.Errorf("dst must have at least one entry")
	}

	// Sort IPs so index→backend assignment is deterministic across calls.
	ips := make([]string, 0, len(dst))
	for ip := range dst {
		ips = append(ips, ip)
	}
	sort.Strings(ips)

	elements := make([]nftables.SetElement, 0, len(ips))
	for i, ipStr := range ips {
		dstPort := dst[ipStr]
		dstIP := net.ParseIP(ipStr).To4()
		if dstIP == nil {
			return nil, fmt.Errorf("dst key %q is not a valid IPv4 address", ipStr)
		}

		// Pack addr+port into 8 bytes (port zero-padded to 4 bytes).
		val := make([]byte, 8)
		copy(val[0:4], dstIP)
		binary.BigEndian.PutUint16(val[4:6], uint16(dstPort))
		// val[6:8] stays zero (padding)

		elements = append(elements, nftables.SetElement{
			Key: binaryutil.NativeEndian.PutUint32(uint32(i)),
			Val: val,
		})
	}

	// ── 1. Build the numgen→(ipv4_addr . inet_service) map ────────────────
	//
	// Key:   TypeInteger (4 bytes, NATIVE byte order uint32: 0, 1, 2, …)
	// Value: TypeIPAddr . TypeInetService
	//          [0:4]  IPv4 address  (4 bytes, network order)
	//          [4:6]  port          (2 bytes, big-endian)
	//          [6:8]  zero-padding  (inet_service padded to 4 bytes in concat)
	//
	// The lookup key must use NATIVE byte order: nft_numgen stores its result
	// into the register as a u32 in host byte order, and the kernel compares
	// the register's raw bytes against the set key. Storing keys big-endian
	// (as this code previously did) makes index 0 work — all-zero bytes — and
	// every other index miss on little-endian hardware, so those packets skip
	// the DNAT rule, fall through to the service's drop rule, and only get
	// through on a 1s TCP SYN retransmit. With N backends, only index 0 ever
	// receives traffic.

	var lbMap *nftables.Set
	n := fmt.Sprintf("map_%s_%d", sname, port)
	// If the set already exists (e.g. rule update), delete the old one
	// before recreating it. This avoids EEXIST on batch flush.
	existingSets, err := c.GetSets(table)
	if err != nil {
		return nil, fmt.Errorf("get sets: %w", err)
	}
	for _, s := range existingSets {
		if s.Name == n {
			lbMap = s
			c.FlushSet(lbMap)
			if err := c.SetAddElements(lbMap, elements); err != nil {
				return nil, fmt.Errorf("SetAddElements: %w", err)
			}
			break
		}
	}
	if lbMap == nil {
		lbMap = &nftables.Set{
			Table:    table,
			Name:     fmt.Sprintf("map_%s_%d", sname, port),
			Constant: false,
			IsMap:    true,
			KeyType:  nftables.TypeInteger,
			DataType: nftables.MustConcatSetType(nftables.TypeIPAddr, nftables.TypeInetService),
		}
		// AddSet populates lbMap.ID, which the Lookup expr below must reference.
		if err := c.AddSet(lbMap, elements); err != nil {
			return nil, fmt.Errorf("AddSet: %w", err)
		}
	}

	// ── 2. Resolve protocol ────────────────────────────────────────────────
	var ipProto byte
	var transportOffset uint32
	switch protocol {
	case "tcp":
		ipProto = unix.IPPROTO_TCP
		transportOffset = 2
	case "udp":
		ipProto = unix.IPPROTO_UDP
		transportOffset = 2
	default:
		return nil, fmt.Errorf("unsupported protocol %q (want \"tcp\" or \"udp\")", protocol)
	}

	// ── 3. Compute register layout for concat value ─────────────────────
	//
	// The concat value (ipv4_addr . inet_service) is 8 bytes stored
	// across consecutive 4-byte registers. Count how many register slots
	// the components before the port (the last component) occupy so we
	// can compute the port register offset relative to the base.
	//
	// Kernel register numbering (include/uapi/linux/netfilter/nf_tables.h):
	//   NFT_REG32_00 = 0x8   — first 32-bit (4-byte) data register
	//   NFT_REG32_01 = 0x9   — second 32-bit data register
	//
	// Using the NFT_REG32_* space avoids the non-linear mapping that
	// nft_parse_register() applies to legacy registers (NFT_REG_1..4).
	const regBase = 0x8 // NFT_REG32_00

	components := nftables.ConcatSetTypeElements(lbMap.DataType)
	regSlots := uint32(0)
	for i := 0; i < len(components)-1; i++ {
		// Each component is padded to a 4-byte register boundary.
		regSlots += (components[i].Bytes + 3) / 4 // ceil(Bytes / 4)
	}
	regProto := regBase + regSlots

	// ── 4. Expression pipeline ─────────────────────────────────────────────
	//
	// Register plan:
	//   reg 1    scratch for payload/meta/numgen expressions
	//   reg 8    after Lookup: dest IPv4 address (NFT_REG32_00)
	//   reg 9    after Lookup: dest port (NFT_REG32_01)
	//
	//   payload(net hdr, off=16, len=4) → reg1 = daddr
	//   cmp eq reg1 extIP
	//   meta L4PROTO                    → reg1 = proto
	//   cmp eq reg1 ipProto
	//   payload(trans hdr, off=2, len=2)→ reg1 = dport
	//   cmp eq reg1 port
	//   numgen inc mod N                → reg1 = 0..N-1
	//   lookup src=reg1 → lbMap        → reg8=addr, reg9=port
	//   nat dnat addr=reg8, proto=reg9

	return []expr.Any{
		// ip daddr <extIP>
		&expr.Payload{
			DestRegister: 0x1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       0x10, // IPv4 daddr
			Len:          0x4,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 0x1,
			Data:     extIP.To4(),
		},

		// <protocol>
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 0x1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 0x1,
			Data:     []byte{ipProto},
		},

		// dport <port>
		&expr.Payload{
			DestRegister: 0x1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       transportOffset,
			Len:          0x2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 0x1,
			Data:     binaryutil.BigEndian.PutUint16(port),
		},

		// // numgen inc mod N → reg1
		&expr.Numgen{
			Register: 0x1,
			Modulus:  uint32(len(dst)),
			Type:     unix.NFT_NG_INCREMENTAL,
		},

		// map lookup → regBase (addr) + regProto (port)
		// IsDestRegSet=true → map lookup (vs plain membership test)
		// concat value written across consecutive registers
		&expr.Lookup{
			SourceRegister: 0x1,
			DestRegister:   regBase,
			IsDestRegSet:   true,
			SetName:        lbMap.Name,
			SetID:          lbMap.ID,
		},

		// dnat to regBase:regProto
		// RegAddrMin==RegAddrMax and RegProtoMin==RegProtoMax → single
		// The counter must precede the NAT statement: NAT terminates rule
		// evaluation, so a counter placed after it never runs and every
		// per-rule counter reads zero even while traffic flows. Tests and
		// operators rely on these counters to tell "no traffic" apart from
		// "traffic dropped".
		&expr.Counter{},

		// destination, not a range.
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      unix.NFPROTO_IPV4,
			RegAddrMin:  regBase,
			RegProtoMin: regProto,
		},

		&expr.Verdict{
			Kind: expr.VerdictAccept,
		},
	}, nil
}

// setStore is the subset of *nftables.Conn used to materialise geo sets. It
// exists so the union-set logic can be unit-tested without a netlink socket.
type setStore interface {
	GetSets(table *nftables.Table) ([]*nftables.Set, error)
	AddSet(set *nftables.Set, elements []nftables.SetElement) error
	FlushSet(set *nftables.Set)
	SetAddElements(set *nftables.Set, elements []nftables.SetElement) error
	DelSet(set *nftables.Set)
	Flush() error
}

func geoSetName(sn string) string { return geoSetPrefix + sn }

type ipRange struct {
	first uint32
	last  uint32
}

// geoRanges normalises CIDRs into sorted, disjoint, inclusive IPv4 ranges.
// GeoLite2 countries are globally disjoint, but merging keeps the set compact
// and protects against overlapping input (the kernel rejects duplicate
// interval elements with EEXIST).
func geoRanges(cidrs []*net.IPNet) []ipRange {
	var ranges []ipRange
	for _, cidr := range cidrs {
		if cidr == nil {
			continue
		}
		ip4 := cidr.IP.To4()
		if ip4 == nil {
			continue
		}
		ones, bits := cidr.Mask.Size()
		if bits != 32 {
			continue
		}
		first := binary.BigEndian.Uint32(ip4.Mask(cidr.Mask))
		size := uint64(1) << uint(bits-ones)
		last := uint64(first) + size - 1
		ranges = append(ranges, ipRange{first: first, last: uint32(last)})
	}
	if len(ranges) == 0 {
		return nil
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].first == ranges[j].first {
			return ranges[i].last < ranges[j].last
		}
		return ranges[i].first < ranges[j].first
	})

	merged := ranges[:1]
	for _, r := range ranges[1:] {
		cur := &merged[len(merged)-1]
		// Merge overlapping and adjacent ranges.
		if r.first <= cur.last || (cur.last != ^uint32(0) && r.first == cur.last+1) {
			if r.last > cur.last {
				cur.last = r.last
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// geoSetElements converts CIDRs into nftables interval-set elements.
func geoSetElements(cidrs []*net.IPNet) []nftables.SetElement {
	ranges := geoRanges(cidrs)
	elements := make([]nftables.SetElement, 0, len(ranges)*2)
	for _, r := range ranges {
		first := binary.BigEndian.AppendUint32(nil, r.first)
		if r.first == r.last {
			elements = append(elements, nftables.SetElement{Key: first})
			continue
		}
		elements = append(elements,
			nftables.SetElement{Key: first},
			nftables.SetElement{
				Key:         binary.BigEndian.AppendUint32(nil, r.last),
				IntervalEnd: true,
			},
		)
	}
	return elements
}

// geoSetChunkSize bounds the number of set elements per netlink message.
// A whole-country union (for example US+CA) easily exceeds the kernel socket
// send buffer: a single 20k-element message fails with EMSGSIZE
// ("sendmsg: message too long"). 2k keeps each message well under the limit,
// at the cost of a handful of round-trips per refresh.
const geoSetChunkSize = 2000

// buildGeoUnionSet creates or refreshes the per-service union set
// geo_<safeName(serviceKey)>.
func buildGeoUnionSet(ss setStore, table *nftables.Table, sn string, cidrs []*net.IPNet) (*nftables.Set, error) {
	name := geoSetName(sn)
	elements := geoSetElements(cidrs)

	existing, err := ss.GetSets(table)
	if err != nil {
		return nil, fmt.Errorf("get sets: %w", err)
	}
	for _, s := range existing {
		if s.Name != name {
			continue
		}
		// Flush and repopulate separately: the element list may be too large
		// for a single message, and SetAddElements cannot split it.
		ss.FlushSet(s)
		if err := ss.Flush(); err != nil {
			return nil, fmt.Errorf("flush set %s: %w", name, err)
		}
		return s, addSetElementsChunked(ss, s, elements)
	}

	set := &nftables.Set{
		Table:    table,
		Name:     name,
		KeyType:  nftables.TypeIPAddr,
		Interval: true,
	}
	// Create the set empty first so element chunks can be sent in their own
	// messages; AddSet with a full element list would reintroduce EMSGSIZE.
	if err := ss.AddSet(set, nil); err != nil {
		return nil, fmt.Errorf("AddSet(%s): %w", name, err)
	}
	if err := ss.Flush(); err != nil {
		return nil, fmt.Errorf("create set %s: %w", name, err)
	}
	if err := addSetElementsChunked(ss, set, elements); err != nil {
		return nil, err
	}
	return set, nil
}

// addSetElementsChunked adds elements in geoSetChunkSize batches, flushing
// each batch. It is a no-op for an empty set.
func addSetElementsChunked(ss setStore, set *nftables.Set, elements []nftables.SetElement) error {
	for start := 0; start < len(elements); start += geoSetChunkSize {
		end := start + geoSetChunkSize
		if end > len(elements) {
			end = len(elements)
		}
		if err := ss.SetAddElements(set, elements[start:end]); err != nil {
			return fmt.Errorf("SetAddElements(%s) [%d:%d]: %w", set.Name, start, end, err)
		}
		if err := ss.Flush(); err != nil {
			return fmt.Errorf("flush set %s [%d:%d]: %w", set.Name, start, end, err)
		}
	}
	return nil
}

// buildGeoSetDenyExprs returns:
//
//	ip daddr <extIP> ip saddr not in @<geoSet> counter drop
//
// The deny verdict must precede the DNAT rules: an accept in nat prerouting
// terminates chain evaluation, which would invert the allow-list (ADR 0001).
func buildGeoSetDenyExprs(extIP net.IP, set *nftables.Set) []expr.Any {
	return []expr.Any{
		&expr.Payload{
			DestRegister: 0x1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       0x10, // IPv4 daddr
			Len:          0x4,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 0x1,
			Data:     extIP.To4(),
		},
		&expr.Payload{
			DestRegister: 0x1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       0x0c, // IPv4 saddr
			Len:          0x4,
		},
		&expr.Lookup{
			SourceRegister: 0x1,
			SetName:        set.Name,
			SetID:          set.ID,
			Invert:         true,
		},
		&expr.Counter{},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}
