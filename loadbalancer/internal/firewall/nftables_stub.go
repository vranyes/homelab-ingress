//go:build !linux

package firewall

import (
	"fmt"
	"log/slog"
)

const tableName = "homelab_lb"

// chainName converts a service key to a valid nftables chain name.
// Defined here so non-Linux builds can still reference it (e.g. in tests
// that import the firewall package without a Linux build tag).
func chainName(serviceKey string) string {
	safe := make([]byte, 0, len(serviceKey))
	for _, c := range serviceKey {
		switch c {
		case '/', '-':
			safe = append(safe, '_')
		default:
			safe = append(safe, byte(c))
		}
	}
	s := string(safe)
	if len(s) > 30 {
		s = s[:30]
	}
	return "ingress_" + s
}

// Manager is a no-op firewall manager for non-Linux platforms.
type Manager struct {
	logger *slog.Logger
}

func NewManager() *Manager                              { return NewManagerWithLogger(slog.Default()) }
func NewManagerWithLogger(logger *slog.Logger) *Manager { return &Manager{logger: logger} }
func NewNoOpManager() *Manager                          { return NewManager() }

func (m *Manager) AddRules(iface, extIP string, extPort int, protocol string, podIPs []string, podPort int, serviceKey string) error {
	return m.AddServiceRules(iface, extIP, []PortRule{{ExtPort: extPort, Protocol: protocol, PodIPs: append([]string(nil), podIPs...), PodPort: podPort}}, serviceKey)
}

// PortRule describes a single external-port → pod DNAT mapping.
type PortRule struct {
	ExtPort  int
	Protocol string
	PodIPs   []string
	PodPort  int
}

func (m *Manager) AddServiceRules(_ string, _ string, ports []PortRule, _ string) error {
	if len(ports) == 0 {
		return fmt.Errorf("no ports provided")
	}
	for _, p := range ports {
		if len(p.PodIPs) == 0 {
			return fmt.Errorf("no pod IPs provided for ext_port %d", p.ExtPort)
		}
	}
	return nil
}

func (m *Manager) RemoveRules(_ string) error { return nil }
func (m *Manager) RemoveAllRules() error      { return nil }
