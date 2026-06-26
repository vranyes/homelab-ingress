package nftables

import (
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
)

// buildCtStatusAcceptExprs returns expressions for:
//
//	ct status <mask> accept
//
// ct status dnat is set on every packet belonging to a DNAT'd connection,
// including the very first packet (unlike ct state which only tracks
// established/related).
func buildCtStatusAcceptExprs(statusBits uint32) []expr.Any {
	return []expr.Any{
		&expr.Ct{Key: expr.CtKeySTATUS, Register: 1},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(statusBits),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{
			Op:       expr.CmpOpNeq,
			Register: 1,
			Data:     binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// buildMasqueradeWithDevicesExprs returns expressions for:
//
//	iifname "inputDev" oifname "outputDev" ct status dnat masquerade
//
// Masquerade rewrites the source address of forwarded packets to the router's
// outbound IP. The iifname/oifname guards restrict this to traffic crossing
// between specific interfaces (same-L2 or routed), while ct status dnat
// ensures only packets belonging to DNAT'd connections are affected.
func buildMasqueradeWithDevicesExprs(inputDev, outputDev string) []expr.Any {
	return []expr.Any{
		// iifname "inputDev"
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte(inputDev + "\x00"),
		},
		// oifname "outputDev"
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte(outputDev + "\x00"),
		},
		// ct status dnat
		&expr.Ct{Key: expr.CtKeySTATUS, Register: 1},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(CtStatusDNAT),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{
			Op:       expr.CmpOpNeq,
			Register: 1,
			Data:     binaryutil.NativeEndian.PutUint32(0),
		},
		// masquerade
		&expr.Masq{},
	}
}

// buildMasqueradeExprs returns expressions for:
//
//	ct status <type> masquerade
//
// Masquerade rewrites the source address of forwarded packets to the router's
// outbound IP, so that backends (which may not have a route to the original
// LAN client) can reply correctly via the router.
//
// ct status dnat keeps this surgical: only packets belonging to connections
// that were DNAT'd by our prerouting rules are affected. Intra-LAN traffic
// that was never DNAT'd is left untouched.
func buildMasqueradeExprs(natType uint32) []expr.Any {
	return []expr.Any{
		// ct status dnat  (only packets that went through your DNAT rules)
		&expr.Ct{Key: expr.CtKeySTATUS, Register: 1},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(natType),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{
			Op:       expr.CmpOpNeq,
			Register: 1,
			Data:     binaryutil.NativeEndian.PutUint32(0),
		},

		&expr.Masq{},
	}
}

// buildCtStateAcceptExprs returns expressions for:
//
//	ct state <mask> accept
//
// Uses Bitwise to mask the state register then Cmp!=0 to test any bit is set.
func buildCtStateAcceptExprs(stateBits uint32) []expr.Any {
	return []expr.Any{
		&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(stateBits),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{
			Op:       expr.CmpOpNeq,
			Register: 1,
			Data:     binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}
