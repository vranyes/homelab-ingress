//go:build linux

package firewall

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

const tableName = "homelab_lb"

// PortRule describes a single external-port → pod DNAT mapping.
type PortRule struct {
	ExtPort  int
	Protocol string
	PodIPs   []string
	PodPort  int
}

// ingressChainName converts a service key to a valid nftables chain name.
func ingressChainName(serviceKey string) string {
	safe := strings.ReplaceAll(serviceKey, "/", "_")
	safe = strings.ReplaceAll(safe, "-", "_")
	if len(safe) > 30 {
		safe = safe[:30]
	}
	return "ingress_" + safe
}

// forwardChainName converts a service key to a valid nftables chain name.
func forwardChainName(serviceKey string) string {
	safe := strings.ReplaceAll(serviceKey, "/", "_")
	safe = strings.ReplaceAll(safe, "-", "_")
	if len(safe) > 30 {
		safe = safe[:30]
	}
	return "forward_" + safe
}

// Manager owns a nftables connection and applies load-balancer NAT rules.
type Manager struct {
	conn   *nftables.Conn
	mu     sync.Mutex
	logger *slog.Logger
}

// NewManager constructs a Manager using the default nftables connection.
func NewManager() *Manager {
	return NewManagerWithLogger(slog.Default())
}

// NewManagerWithLogger constructs a Manager with a custom logger.
func NewManagerWithLogger(logger *slog.Logger) *Manager {
	conn, err := nftables.New()
	if err != nil {
		logger.Error("nftables unavailable, firewall management disabled", "error", err)
	}
	return &Manager{conn: conn, logger: logger}
}

// NewNoOpManager returns a Manager that silently succeeds all operations.
// Intended for testing environments where nftables is not available.
func NewNoOpManager() *Manager {
	return &Manager{conn: nil, logger: slog.Default()}
}

// newManagerWithConn builds a Manager from an existing connection; used in tests.
func newManagerWithConn(conn *nftables.Conn, logger *slog.Logger) *Manager {
	return &Manager{conn: conn, logger: logger}
}

// AddServiceRules installs DNAT rules for all ports of a service atomically.
// A single chain is created for the service and rules for every port are added
// in one Flush call, preventing the overwrite issue that occurs when AddRules
// is called in a loop (each call would delete and recreate the chain).
func (m *Manager) AddServiceRules(iface, extIP string, ports []PortRule, serviceKey string) error {
	m.logger.Info("adding firewall rules", "service", serviceKey, "iface", iface, "ports", len(ports))
	if len(extIP) == 0 {
		return fmt.Errorf("external IP is required")
	}
	if len(ports) == 0 {
		return fmt.Errorf("no ports provided")
	}
	for _, p := range ports {
		if len(p.PodIPs) == 0 {
			return fmt.Errorf("no pod IPs provided for ext_port %d", p.ExtPort)
		}
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

	chain := ingressChainName(serviceKey)

	// Remove existing chains once before recreating with all rules.
	_ = m.removeServiceChain(table, chain)
	_ = m.removeServiceChain(table, forwardChainName(serviceKey))

	// Priority -110: runs before inet fw4's dstnat chain (-100).
	prio := nftables.ChainPriorityRef(-110)
	policy := nftables.ChainPolicyAccept
	svcChain := m.conn.AddChain(&nftables.Chain{
		Table:    table,
		Name:     chain,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: prio,
		Policy:   &policy,
	})

	// Create a forward filter chain for post-DNAT acceptance of pod IPs/ports.
	// Priority -10: runs early in forward hook.
	fwdPrio := nftables.ChainPriorityRef(-10)
	fwdChainName := forwardChainName(serviceKey)
	fwdChain := m.conn.AddChain(&nftables.Chain{
		Table:    table,
		Name:     fwdChainName,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: fwdPrio,
		Policy:   &policy,
	})

	// Add one nftables rule per port to the same chain in a single batch.
	for i, port := range ports {
		exprs := matchExprs(iface, extIP, port.ExtPort, port.Protocol)
		if len(port.PodIPs) == 1 {
			exprs = append(exprs, dnatSingleExprs(port.PodIPs[0], port.PodPort)...)
		} else {
			setName := fmt.Sprintf("lb_%s_%d", chain, i)
			_, lookupExprs, err := m.addLBMapNamed(table, setName, port.PodIPs, port.PodPort)
			if err != nil {
				return fmt.Errorf("build LB map for ext_port %d: %w", port.ExtPort, err)
			}
			exprs = append(exprs, lookupExprs...)
		}
		m.conn.AddRule(&nftables.Rule{Table: table, Chain: svcChain, Exprs: exprs})

		// Also add forward-chain accept rules for the (post-DNAT) pod IP(s)/pod port.
		if len(port.PodIPs) == 1 {
			fwdExprs := append(matchIPDstExprs(port.PodIPs[0]), matchProtoPortExprs(port.Protocol, port.PodPort)...)
			fwdExprs = append(fwdExprs, []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}...)
			m.conn.AddRule(&nftables.Rule{Table: table, Chain: fwdChain, Exprs: fwdExprs})
		} else {
			for _, podIP := range port.PodIPs {
				fwdExprs := append(matchIPDstExprs(podIP), matchProtoPortExprs(port.Protocol, port.PodPort)...)
				fwdExprs = append(fwdExprs, []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}...)
				m.conn.AddRule(&nftables.Rule{Table: table, Chain: fwdChain, Exprs: fwdExprs})
			}
		}
	}
	m.conn.AddRule(&nftables.Rule{Table: table, Chain: svcChain, Exprs: []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}})

	if err := m.conn.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	m.logger.Info("firewall rules applied", "service", serviceKey)
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
		m.removeServiceChain(table, ingressChainName(serviceKey)),
		m.removeServiceChain(table, forwardChainName(serviceKey)),
	)
}

// RemoveAllRules deletes the entire homelab_lb table.
func (m *Manager) RemoveAllRules() error {
	m.logger.Info("removing all firewall rules")
	if m.conn == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	table, _ := m.findTable()
	if table == nil {
		return nil
	}
	m.conn.DelTable(table)
	return m.conn.Flush()
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
			return nil
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
	m.conn.AddRule(&nftables.Rule{Table: table, Chain: post, Exprs: []expr.Any{&expr.Masq{}}})
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
		// Ensure set deletions are flushed even if the service chain isn't present.
		if err := m.conn.Flush(); err != nil {
			return fmt.Errorf("flush after set deletions: %w", err)
		}
	}

	chains, err := m.conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return fmt.Errorf("list chains: %w", err)
	}
	for _, c := range chains {
		if c.Table.Name == tableName && c.Name == chain {
			m.conn.DelChain(c)
			// Flush again after deleting the chain.
			return m.conn.Flush()
		}
	}
	return nil
}

// addLBMapNamed creates a named integer->(ipv4_addr . inet_service) map for
// round-robin DNAT and returns the expressions to perform the lookup.
func (m *Manager) addLBMapNamed(table *nftables.Table, setName string, podIPs []string, podPort int) (*nftables.Set, []expr.Any, error) {
	dataType := nftables.MustConcatSetType(nftables.TypeIPAddr, nftables.TypeInetService)
	set := &nftables.Set{
		Table:    table,
		Name:     setName,
		KeyType:  nftables.TypeInteger,
		DataType: dataType,
		IsMap:    true,
	}

	elements := make([]nftables.SetElement, len(podIPs))
	for i, ip := range podIPs {
		parsed := net.ParseIP(ip).To4()
		if parsed == nil {
			return nil, nil, fmt.Errorf("invalid pod IP %q", ip)
		}
		// Value: 4-byte IPv4 address followed by 2-byte port padded to 4 bytes.
		val := make([]byte, 8)
		copy(val[0:4], parsed)
		binary.BigEndian.PutUint16(val[4:6], uint16(podPort))
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
		&expr.Numgen{Register: 1, Type: unix.NFT_NG_INCREMENTAL, Modulus: uint32(len(podIPs))},
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
func matchExprs(_, extIP string, extPort int, protocol string) []expr.Any {
	var exprs []expr.Any
	if extIP != "" {
		exprs = append(exprs, matchIPDstExprs(extIP)...)
	}
	exprs = append(exprs, matchProtoPortExprs(protocol, extPort)...)
	return exprs
}

func matchIPDstExprs(ip string) []expr.Any {
	return []expr.Any{
		// IPv4 header: destination address at offset 16, length 4.
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: net.ParseIP(ip).To4()},
	}
}

func matchProtoPortExprs(protocol string, port int) []expr.Any {
	var protoNum byte
	if protocol == "udp" {
		protoNum = unix.IPPROTO_UDP
	} else {
		protoNum = unix.IPPROTO_TCP
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protoNum}},
		// Transport header: destination port at offset 2, length 2.
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: bigEndianUint16(uint16(port))},
	}
}

func dnatSingleExprs(ip string, port int) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: net.ParseIP(ip).To4()},
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
