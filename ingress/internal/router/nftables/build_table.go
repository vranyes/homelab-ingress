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

	// ── 1. Build the numgen→(ipv4_addr . inet_service) map ────────────────
	//
	// Key:   TypeMark   (4 bytes, network-byte-order uint32: 0, 1, 2, …)
	// Value: TypeIPAddr . TypeInetService
	//          [0:4]  IPv4 address  (4 bytes, network order)
	//          [4:6]  port          (2 bytes, big-endian)
	//          [6:8]  zero-padding  (inet_service padded to 4 bytes in concat)
	//
	// NOTE: using TypeMark rather than TypeInteger because the kernel expects
	// integer-type keys in host byte order, but the nftables Go library
	// encodes them with BigEndian. On little-endian (x86) hardware, element 1
	// gets stored as 0x01000000 (16777216) while numgen outputs 1 → lookup
	// fails → packet misses DNAT. TypeMark uses network byte order, which
	// matches BigEndian encoding.
	lbMap := &nftables.Set{
		Table:    table,
		Name:     fmt.Sprintf("map_%s_%d", sname, port),
		Constant: false,
		IsMap:    true,
		KeyType:  nftables.TypeMark,
		DataType: nftables.MustConcatSetType(nftables.TypeIPAddr, nftables.TypeInetService),
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
			Key: binaryutil.BigEndian.PutUint32(uint32(i)),
			Val: val,
		})
	}

	// AddSet populates lbMap.ID, which the Lookup expr below must reference.
	if err := c.AddSet(lbMap, elements); err != nil {
		return nil, fmt.Errorf("AddSet: %w", err)
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

	// ── 3. Expression pipeline ─────────────────────────────────────────────
	//
	// Register plan:
	//   reg 1  scratch → numgen output → after Lookup: dest IPv4 address
	//   reg 2  after Lookup: dest port (concat value spills into reg 2)
	//
	//   payload(net hdr, off=16, len=4) → reg1 = daddr
	//   cmp eq reg1 extIP
	//   meta L4PROTO                    → reg1 = proto
	//   cmp eq reg1 ipProto
	//   payload(trans hdr, off=2, len=2)→ reg1 = dport
	//   cmp eq reg1 port
	//   numgen inc mod N                → reg1 = 0..N-1
	//   lookup src=reg1 → lbMap        → reg1=addr, reg2=port
	//   nat dnat addr=reg1, proto=reg2

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

		// map lookup → reg1 (addr) + reg2 (port)
		// IsDestRegSet=true → map lookup (vs plain membership test)
		// concat value written across consecutive registers
		&expr.Lookup{
			SourceRegister: 0x1,
			DestRegister:   0x1,
			IsDestRegSet:   true,
			SetName:        lbMap.Name,
			SetID:          lbMap.ID,
		},

		// dnat to reg1:reg2
		// RegAddrMin==RegAddrMax and RegProtoMin==RegProtoMax → single
		// destination, not a range.
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      unix.NFPROTO_IPV4,
			RegAddrMin:  0x1,
			RegProtoMin: 0x9,
		},

		&expr.Verdict{
			Kind: expr.VerdictAccept,
		},
	}, nil
}
