//go:build linux

package nftables

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

type Manager struct {
	conn   *nftables.Conn
	mu     sync.Mutex
	logger *slog.Logger
}

func NewManager(logger *slog.Logger, opts ...Option) *Manager {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	conn, err := nftables.New(nftables.WithSockOptions(func(conn *netlink.Conn) error {
		return conn.SetOption(netlink.ExtendedAcknowledge, true)
	}))

	if err != nil {
		logger.Error("nftables unavailable, firewall management disabled", "error", err)
		return &Manager{logger: logger}
	}

	var table *nftables.Table
	tables, err := conn.ListTables()
	if err != nil {
		logger.Error("failed to list nftables tables", "error", err)
		return &Manager{logger: logger}
	}
	for _, t := range tables {
		if t.Name == tableName {
			table = t
			logger.Info("nftables table found, deleting existing table", "table", tableName)
			conn.DelTable(table)
		}
	}
	logger.Info("nftables table not found, creating new table")
	table = &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	conn.AddTable(table)

	acceptPolicy := nftables.ChainPolicyAccept
	// dropPolicy := nftables.ChainPolicyDrop

	// Adds forward chain
	// filter hook forward priority -1; policy accept;
	forward := conn.AddChain(&nftables.Chain{
		Name:     "forward",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityRef(-10),
		Policy:   &acceptPolicy,
	})

	// Adds the rule
	// ct state established,related accept
	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: forward,
		Exprs: buildCtStateAcceptExprs(
			expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED,
		),
	})

	// Adds the rule
	// `ct status dnat accept`
	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: forward,
		Exprs: buildCtStatusAcceptExprs(CtStatusDNAT),
	})

	// Adds postrouting chain
	// type nat hook postrouting priority srcnat; policy accept;
	postChain := conn.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    table,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityRef(90),
		Type:     nftables.ChainTypeNAT,
		Policy:   &acceptPolicy,
	})

	// Adds masquerade rules to the postrouting chain
	// `iifname "inDev" oifname "outDev" ct status dnat masquerade`
	for _, mr := range o.masqueradeRules {
		logger.Info("adding masquerade rule",
			"input_device", mr.InputDevice,
			"output_device", mr.OutputDevice,
		)
		conn.AddRule(&nftables.Rule{
			Table: table,
			Chain: postChain,
			Exprs: buildMasqueradeWithDevicesExprs(mr.InputDevice, mr.OutputDevice),
		})
	}

	if err := conn.Flush(); err != nil {
		logger.Error("failed to flush nftables rules", "error", err)
		return &Manager{logger: logger}
	}

	return &Manager{conn: conn, logger: logger}
}

func (m *Manager) ListRules() ([]string, error) {
	if m.conn == nil {
		return nil, nil
	}

	table, err := m.findTable()
	if err != nil {
		return nil, fmt.Errorf("find table: %w", err)
	}
	if table == nil {
		return nil, nil
	}

	chains, err := m.conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, fmt.Errorf("list chains: %w", err)
	}

	var services []string
	prefix := "ingress_"
	for _, c := range chains {
		if c.Table.Name == tableName && strings.HasPrefix(c.Name, prefix) {
			svc := strings.TrimPrefix(c.Name, prefix)
			if svc != "" {
				services = append(services, svc)
			}
		}
	}

	return services, nil
}

func (m *Manager) AddRules(service state.LBService) error {
	m.logger.Debug("adding firewall rules", "service", service.ServiceName)
	if err := validateService(service); err != nil {
		return err
	}
	if m.conn == nil {
		return fmt.Errorf("nftables connection not available")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	sn := safeName(service.ServiceName)
	pCName := "pre_" + sn

	table, err := m.findTable()
	if err != nil {
		return fmt.Errorf("find table: %w", err)
	}

	// Create new chains.
	pc, err := m.findChain(table, pCName)
	if err != nil {
		return fmt.Errorf("find chain: %w", err)
	}

	if pc == nil {
		m.logger.Info("creating new chain for service", "chain", pCName, "service", service.ServiceName)
		// pd := nftables.ChainPolicyDrop
		pc = m.conn.AddChain(&nftables.Chain{
			Name:     pCName,
			Table:    table,
			Type:     nftables.ChainTypeNAT,
			Hooknum:  nftables.ChainHookPrerouting,
			Priority: nftables.ChainPriorityRef(-110),
			// Policy:   &pd,
			// Device:   "lan2",
		})
	} else {
		m.logger.Info("chain already exists, flushing existing rules", "chain", pc.Name)
		m.conn.FlushChain(pc)
	}

	extIP := service.SourceAddress
	for _, target := range service.Targets {
		var mapping = make(map[string]uint16, len(target.Dst))
		for _, d := range target.Dst {
			mapping[d.Address.String()] = d.Port
		}

		expr, err := buildRoundRobinExprs(
			m.conn, sn, pc.Table, extIP, target.Protocol, target.SrcPort, mapping,
		)
		if err != nil {
			return fmt.Errorf("build round robin exprs: %w", err)
		}
		m.conn.AddRule(&nftables.Rule{
			Table: table,
			Chain: pc,
			Exprs: expr,
		})
	}

	m.conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: pc,
		Exprs: []expr.Any{
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
			&expr.Verdict{Kind: expr.VerdictDrop},
		}})

	m.logger.Info("firewall rules updated, flushing", "service", service.ServiceName)
	return m.conn.Flush()
}

// RemoveRules removes the chain and any associated LB map for serviceKey.
func (m *Manager) RemoveRules(serviceKey string) error {
	m.logger.Info("removing firewall rules", "service", serviceKey)

	m.mu.Lock()
	defer m.mu.Unlock()

	pfix := safeName(serviceKey)

	table, _ := m.findTable()
	if table == nil {
		return nil
	}

	chains, err := m.conn.ListChains()
	if err != nil {
		return fmt.Errorf("list chains: %w", err)
	}
	for _, c := range chains {
		if c.Table.Name == tableName && strings.Contains(c.Name, pfix) {
			m.conn.DelChain(c)
		}
	}

	sets, err := m.conn.GetSets(table)
	if err != nil {
		return fmt.Errorf("get sets: %w", err)
	}
	for _, s := range sets {
		if strings.Contains(s.Name, pfix) {
			m.conn.DelSet(s)
		}
	}

	return m.conn.Flush()
}

// Cleanup removes the entire nftables table and all its rules.
func (m *Manager) Cleanup() {
	if m.conn == nil {
		return
	}
	table, err := m.findTable()
	if err != nil {
		m.logger.Debug("nftables table not found for cleanup", "error", err)
		return
	}
	m.logger.Info("cleaning up nftables table", "table", tableName)
	m.conn.DelTable(table)
	if err := m.conn.Flush(); err != nil {
		m.logger.Error("failed to flush nftables table deletion", "error", err)
	}
}

// -- internal helpers --

func (m *Manager) findTable() (*nftables.Table, error) {
	if m.conn == nil {
		return nil, fmt.Errorf("nftables connection not available")
	}
	tables, err := m.conn.ListTables()
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	for _, t := range tables {
		if t.Name == tableName {
			return t, nil
		}
	}
	return nil, fmt.Errorf("table %s not found", tableName)
}

func (m *Manager) findChain(table *nftables.Table, name string) (*nftables.Chain, error) {
	chains, err := m.conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, err
	}
	for _, c := range chains {
		if c.Table.Name == tableName && c.Name == name {
			return c, nil
		}
	}
	return nil, nil
}
