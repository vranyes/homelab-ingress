//go:build linux

package nftables

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vranyes/homelab-ingress/ingress/internal/state"
	"golang.org/x/sys/unix"
)

// Manager owns a nftables connection and applies ingress NAT rules.
type Manager struct {
	conn   *nftables.Conn
	mu     sync.Mutex
	logger *slog.Logger
}

func NewManager(logger *slog.Logger) *Manager {
	conn, err := nftables.New()
	if err != nil {
		logger.Error("nftables unavailable, firewall management disabled", "error", err)
	}
	return &Manager{conn: conn, logger: logger}
}

func (m *Manager) ListRules() ([]string, error) {
	return nil, nil
}

// AddRules installs DNAT rules for all ports of a service atomically.
// A single chain is created for the service and rules for every port are added
// in one Flush call, preventing the overwrite issue that occurs when AddRules
// is called in a loop (each call would delete and recreate the chain).
func (m *Manager) AddRules(service state.LBService) error {
	m.logger.Info("adding firewall rules", "service", service.ServiceName)
	if err := validateService(service); err != nil {
		return err
	}
	if m.conn == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	table, err := m.ensureTable()
	if err != nil {
		return fmt.Errorf("ensure table: %w", err)
	}
	if err := m.ensurePostrouting(table); err != nil {
		return fmt.Errorf("ensure postrouting: %w", err)
	}

	chainIngress := chainName("ingress", service.ServiceName)
	chainForward := chainName("forward", service.ServiceName)

	needsUpdate, err := m.rulesNeedUpdate(table, service, chainIngress, chainForward)
	if err != nil {
		return fmt.Errorf("check rules: %w", err)
	}
	if !needsUpdate {
		m.logger.Debug("rules already up to date, skipping", "service", service.ServiceName)
		return nil
	}

	// Remove existing chains once before recreating with all rules.
	_ = m.removeServiceChain(table, chainIngress)
	_ = m.removeServiceChain(table, chainForward)

	prio := nftables.ChainPriorityRef(priority)
	policy := nftables.ChainPolicyAccept
	svcChain := m.conn.AddChain(&nftables.Chain{
		Table:    table,
		Name:     chainIngress,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: prio,
		Policy:   &policy,
	})

	// Create a forward filter chain for post-DNAT acceptance of pod IPs/ports.
	// Priority -10: runs early in forward hook.
	fwdPrio := nftables.ChainPriorityRef(-10)
	fwdChain := m.conn.AddChain(&nftables.Chain{
		Table:    table,
		Name:     chainForward,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: fwdPrio,
		Policy:   &policy,
	})

	extIP := service.SourceAddress.String()

	// Add one nftables rule per target to the same chain in a single batch.
	for i, target := range service.Targets {
		exprs := matchExprs(extIP, target.SrcPort, target.Protocol)
		exprs = append(exprs, ctMarkSetExprs()...)
		if len(target.Dst) == 1 {
			dest := target.Dst[0]
			exprs = append(exprs, dnatSingleExprs(dest.Address, dest.Port)...)
		} else {
			setName := fmt.Sprintf("lb_%s_%d", chainIngress, i)
			_, lookupExprs, err := m.addLBMapNamed(table, setName, target.Dst)
			if err != nil {
				return fmt.Errorf("build LB map for ext_port %d: %w", target.SrcPort, err)
			}
			exprs = append(exprs, lookupExprs...)
		}
		m.conn.AddRule(&nftables.Rule{Table: table, Chain: svcChain, Exprs: exprs})

		// Also add forward-chain accept rules for the (post-DNAT) destination IP(s)/port.
		// Forward direction: client → pod (matches on daddr + dport)
		// Reply direction: pod → client (matches on saddr + sport)
		for _, dest := range target.Dst {
			fwdExprs := append(matchIPDstExprs(dest.Address), matchProtoPortExprs(target.Protocol, dest.Port)...)
			fwdExprs = append(fwdExprs, []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}...)
			m.conn.AddRule(&nftables.Rule{Table: table, Chain: fwdChain, Exprs: fwdExprs})

			replyExprs := append(matchIPSrcExprs(dest.Address), matchProtoSrcPortExprs(target.Protocol, dest.Port)...)
			replyExprs = append(replyExprs, []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}...)
			m.conn.AddRule(&nftables.Rule{Table: table, Chain: fwdChain, Exprs: replyExprs})
		}
	}
	m.conn.AddRule(&nftables.Rule{Table: table, Chain: svcChain, Exprs: []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}})

	if err := m.conn.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	m.logger.Info("firewall rules applied", "service", service.ServiceName)
	return nil
}

// RemoveRules removes the chain and any associated LB map for serviceKey.
func (m *Manager) RemoveRules(serviceKey string) error {
	m.logger.Info("removing firewall rules", "service", serviceKey)
	if m.conn == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	table, _ := m.findTable()
	if table == nil {
		return nil
	}

	return errors.Join(
		m.removeServiceChain(table, chainName("ingress", serviceKey)),
		m.removeServiceChain(table, chainName("forward", serviceKey)),
	)
}

// -- internal helpers --

func (m *Manager) findTable() (*nftables.Table, error) {
	tables, err := m.conn.ListTables()
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	for _, t := range tables {
		if t.Name == tableName {
			return t, nil
		}
	}
	return nil, nil
}

func (m *Manager) ensureTable() (*nftables.Table, error) {
	t, err := m.findTable()
	if err != nil {
		return nil, err
	}
	if t != nil {
		return t, nil
	}
	t = &nftables.Table{Family: nftables.TableFamilyIPv4, Name: tableName}
	m.conn.AddTable(t)
	if err := m.conn.Flush(); err != nil {
		return nil, fmt.Errorf("create table: %w", err)
	}
	return t, nil
}

func (m *Manager) ensurePostrouting(table *nftables.Table) error {
	chains, err := m.conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return fmt.Errorf("list chains: %w", err)
	}
	for _, c := range chains {
		if c.Table.Name == tableName && c.Name == "postrouting" {
			m.conn.DelChain(c)
			break
		}
	}
	policy := nftables.ChainPolicyAccept
	post := m.conn.AddChain(&nftables.Chain{
		Table:    table,
		Name:     "postrouting",
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
		Policy:   &policy,
	})
	m.conn.AddRule(&nftables.Rule{Table: table, Chain: post, Exprs: append(ctMarkGetExprs(), &expr.Masq{})})
	return m.conn.Flush()
}

func (m *Manager) removeServiceChain(table *nftables.Table, chain string) error {
	// Remove all LB maps associated with this chain (best-effort).
	// Covers legacy single map "lb_<chain>" and indexed maps "lb_<chain>_<n>".
	if sets, err := m.conn.GetSets(table); err == nil {
		prefix := "lb_" + chain
		for _, s := range sets {
			if s.Name == prefix || strings.HasPrefix(s.Name, prefix+"_") {
				m.conn.DelSet(s)
			}
		}
	}

	chains, err := m.conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return fmt.Errorf("list chains: %w", err)
	}
	for _, c := range chains {
		if c.Table.Name == tableName && c.Name == chain {
			m.conn.DelChain(c)
			break
		}
	}

	return m.conn.Flush()
}

// rulesNeedUpdate compares the desired service configuration against existing
// nftables rules. Returns true if chains are missing or rules differ.
func (m *Manager) rulesNeedUpdate(table *nftables.Table, service state.LBService, chainIngress, chainForward string) (bool, error) {
	ingressChain, ingressRules, err := m.findChainAndRules(table, chainIngress)
	if err != nil {
		return false, err
	}
	if ingressChain == nil {
		return true, nil
	}

	forwardChain, forwardRules, err := m.findChainAndRules(table, chainForward)
	if err != nil {
		return false, err
	}
	if forwardChain == nil {
		return true, nil
	}

	extIP := service.SourceAddress.String()

	// Check ingress rules (DNAT rules + final accept).
	expectedIngressCount := len(service.Targets) + 1 // +1 for final accept
	if len(ingressRules) != expectedIngressCount {
		return true, nil
	}

	for i, target := range service.Targets {
		rule := ingressRules[i]
		if !m.ruleMatchesTarget(rule, extIP, target) {
			return true, nil
		}

		// For multi-destination targets, verify the LB map exists and has correct elements.
		if len(target.Dst) > 1 {
			setName := fmt.Sprintf("lb_%s_%d", chainIngress, i)
			if !m.setMatchesDestinations(table, setName, target.Dst) {
				return true, nil
			}
		}
	}

	// Check last rule is accept.
	lastIngress := ingressRules[len(ingressRules)-1]
	if !isAcceptRule(lastIngress) {
		return true, nil
	}

	// Check forward rules (forward + reply for each destination).
	expectedForwardCount := 0
	for _, target := range service.Targets {
		expectedForwardCount += len(target.Dst) * 2
	}
	if len(forwardRules) != expectedForwardCount {
		return true, nil
	}

	fwdIdx := 0
	for _, target := range service.Targets {
		for _, dest := range target.Dst {
			if !m.forwardRuleMatches(forwardRules[fwdIdx], dest.Address, target.Protocol, dest.Port, true) {
				return true, nil
			}
			fwdIdx++
			if !m.forwardRuleMatches(forwardRules[fwdIdx], dest.Address, target.Protocol, dest.Port, false) {
				return true, nil
			}
			fwdIdx++
		}
	}

	return false, nil
}

func (m *Manager) findChainAndRules(table *nftables.Table, chainName string) (*nftables.Chain, []nftables.Rule, error) {
	chains, err := m.conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, nil, fmt.Errorf("list chains: %w", err)
	}
	for _, c := range chains {
		if c.Table.Name == tableName && c.Name == chainName {
			rules, err := m.conn.GetRules(table, c)
			if err != nil {
				return c, nil, fmt.Errorf("get rules: %w", err)
			}
			vals := make([]nftables.Rule, len(rules))
			for i, r := range rules {
				vals[i] = *r
			}
			return c, vals, nil
		}
	}
	return nil, nil, nil
}

func (m *Manager) ruleMatchesTarget(rule nftables.Rule, extIP string, target state.LBTarget) bool {
	hasIPMatch := extIP == "" // vacuously true when no IP to match
	hasProtoMatch := false
	hasPortMatch := false
	hasDNAT := false

	for _, e := range rule.Exprs {
		switch ex := e.(type) {
		case *expr.Cmp:
			if !hasIPMatch && len(ex.Data) == 4 {
				parsed := net.ParseIP(extIP).To4()
				if parsed != nil && bytes.Equal(ex.Data, parsed) {
					hasIPMatch = true
				}
			}
			if len(ex.Data) == 1 && ex.Data[0] == protoByte(target.Protocol) {
				hasProtoMatch = true
			}
			if len(ex.Data) == 2 {
				port := int(binary.BigEndian.Uint16(ex.Data))
				if port == target.SrcPort {
					hasPortMatch = true
				}
			}
		case *expr.NAT:
			hasDNAT = true
		}
	}

	return hasIPMatch && hasProtoMatch && hasPortMatch && hasDNAT
}

func (m *Manager) forwardRuleMatches(rule nftables.Rule, ip net.IP, protocol string, port int, isForward bool) bool {
	hasIPMatch := false
	hasProtoMatch := false
	hasPortMatch := false
	hasAccept := false

	for _, e := range rule.Exprs {
		switch ex := e.(type) {
		case *expr.Payload:
			if ex.Base == expr.PayloadBaseNetworkHeader {
				if ex.Offset == 16 && isForward {
					// daddr match
				} else if ex.Offset == 12 && !isForward {
					// saddr match
				}
			}
		case *expr.Cmp:
			if len(ex.Data) == 4 {
				parsed := ip.To4()
				if parsed != nil && bytes.Equal(ex.Data, parsed) {
					hasIPMatch = true
				}
			}
			if len(ex.Data) == 1 && ex.Data[0] == protoByte(protocol) {
				hasProtoMatch = true
			}
			if len(ex.Data) == 2 {
				rulePort := int(binary.BigEndian.Uint16(ex.Data))
				if rulePort == port {
					hasPortMatch = true
				}
			}
		case *expr.Verdict:
			if ex.Kind == expr.VerdictAccept {
				hasAccept = true
			}
		}
	}

	return hasIPMatch && hasProtoMatch && hasPortMatch && hasAccept
}

func (m *Manager) setMatchesDestinations(table *nftables.Table, setName string, dests []state.LBDestination) bool {
	sets, err := m.conn.GetSets(table)
	if err != nil {
		return false
	}
	for _, s := range sets {
		if s.Name == setName {
			elements, err := m.conn.GetSetElements(s)
			if err != nil {
				return false
			}
			if len(elements) != len(dests) {
				return false
			}
			for i, elem := range elements {
				parsed := dests[i].Address.To4()
				if parsed == nil {
					return false
				}
				val := make([]byte, 8)
				copy(val[0:4], parsed)
				binary.BigEndian.PutUint16(val[4:6], uint16(dests[i].Port))
				if !bytes.Equal(elem.Val, val) {
					return false
				}
			}
			return true
		}
	}
	return false
}

func isAcceptRule(rule nftables.Rule) bool {
	for _, e := range rule.Exprs {
		if ex, ok := e.(*expr.Verdict); ok && ex.Kind == expr.VerdictAccept {
			return true
		}
	}
	return false
}

func protoByte(protocol string) byte {
	if protocol == "udp" {
		return unix.IPPROTO_UDP
	}
	return unix.IPPROTO_TCP
}

// addLBMapNamed creates a named integer->(ipv4_addr . inet_service) map for
// round-robin DNAT and returns the expressions to perform the lookup.
func (m *Manager) addLBMapNamed(table *nftables.Table, setName string, dests []state.LBDestination) (*nftables.Set, []expr.Any, error) {
	dataType := nftables.MustConcatSetType(nftables.TypeIPAddr, nftables.TypeInetService)
	set := &nftables.Set{
		Table:    table,
		Name:     setName,
		KeyType:  nftables.TypeInteger,
		DataType: dataType,
		IsMap:    true,
	}

	elements := make([]nftables.SetElement, len(dests))
	for i, dest := range dests {
		parsed := dest.Address.To4()
		if parsed == nil {
			return nil, nil, fmt.Errorf("invalid destination IP %q", dest.Address)
		}
		// Value: 4-byte IPv4 address followed by 2-byte port padded to 4 bytes.
		val := make([]byte, 8)
		copy(val[0:4], parsed)
		binary.BigEndian.PutUint16(val[4:6], uint16(dest.Port))
		elements[i] = nftables.SetElement{
			Key: nativeUint32(uint32(i)),
			Val: val,
		}
	}

	if err := m.conn.AddSet(set, elements); err != nil {
		return nil, nil, fmt.Errorf("add set: %w", err)
	}

	exprs := []expr.Any{
		// numgen inc mod N -> reg 1
		&expr.Numgen{Register: 1, Type: unix.NFT_NG_INCREMENTAL, Modulus: uint32(len(dests))},
		// map lookup: reg 1 -> IP (reg 1) + port (reg 2)
		&expr.Lookup{SourceRegister: 1, DestRegister: 1, IsDestRegSet: true, SetName: set.Name, SetID: set.ID},
		// DNAT to reg1:reg2
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2},
	}
	return set, exprs, nil
}

// -- expression builders --

// matchExprs returns expressions that match the destination IP, protocol, and
// destination port. iifname matching is intentionally omitted: in the prerouting
// hook the kernel may still report the physical parent interface rather than the
// MACVLAN alias, causing rules to be silently skipped. Matching on the unique
// destination IP is sufficient and interface-agnostic.
func matchExprs(extIP string, extPort int, protocol string) []expr.Any {
	var exprs []expr.Any
	if extIP != "" {
		exprs = append(exprs, matchIPDstExprs(net.ParseIP(extIP))...)
	}
	exprs = append(exprs, matchProtoPortExprs(protocol, extPort)...)
	return exprs
}

func ctMarkSetExprs() []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: []byte{ctMarkValue}},
		&expr.Ct{Register: 1, SourceRegister: true, Key: expr.CtKeyMARK},
	}
}

func ctMarkGetExprs() []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeyMARK},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{ctMarkValue}},
	}
}

func matchIPDstExprs(ip net.IP) []expr.Any {
	return []expr.Any{
		// IPv4 header: destination address at offset 16, length 4.
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ip.To4()},
	}
}

func matchProtoPortExprs(protocol string, port int) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protoByte(protocol)}},
		// Transport header: destination port at offset 2, length 2.
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: bigEndianUint16(uint16(port))},
	}
}

func matchIPSrcExprs(ip net.IP) []expr.Any {
	return []expr.Any{
		// IPv4 header: source address at offset 12, length 4.
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ip.To4()},
	}
}

func matchProtoSrcPortExprs(protocol string, port int) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protoByte(protocol)}},
		// Transport header: source port at offset 0, length 2.
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: bigEndianUint16(uint16(port))},
	}
}

func dnatSingleExprs(ip net.IP, port int) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: ip.To4()},
		&expr.Immediate{Register: 2, Data: bigEndianUint16(uint16(port))},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2},
	}
}

// -- byte helpers --

func bigEndianUint16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func nativeUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, v)
	return b
}
