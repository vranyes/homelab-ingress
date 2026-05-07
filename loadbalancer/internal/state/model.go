package state

import (
	"net"
)

const StateVersion = 1

// State holds the persisted router state. It contains an embedded mutex
// that should be used when mutating or reading the Services map.
type State struct {
	Version   int                   `json:"version"`
	BaseIface string                `json:"base_interface"`
	Services  map[string]*LBService `json:"services"`
}

type LBService struct {
	MACAddr      string      `json:"macaddr"`
	Interface    string      `json:"interface"`
	Ports        []PortEntry `json:"ports"`
	CurrentIP    string      `json:"current_ip"`
	DHCPAcquired bool        `json:"dhcp_acquired"`
}

type PortEntry struct {
	Protocol     string   `json:"protocol"`
	ExternalPort int      `json:"external_port"`
	PodPort      int      `json:"pod_port"`
	PodIPs       []string `json:"pod_ips"`
}

func NewState(baseIface string) *State {
	return &State{
		Version:   StateVersion,
		BaseIface: baseIface,
		Services:  make(map[string]*LBService),
	}
}

func (s *LBService) ParseMAC() (net.HardwareAddr, error) {
	return net.ParseMAC(s.MACAddr)
}

func (s *LBService) SetMAC(mac net.HardwareAddr) {
	s.MACAddr = mac.String()
}

// Clone returns a deep copy of the state suitable for callers that want
// to iterate or marshal a consistent snapshot without holding the lock.
func (s *State) Clone() *State {
	// Use package-level mutex to snapshot a consistent view. The package
	// mutex is used by other helpers (UpdateService/UpdateIP/Save) so this
	// keeps synchronization consistent.
	mu.RLock()
	defer mu.RUnlock()

	copyState := &State{
		Version:   s.Version,
		BaseIface: s.BaseIface,
		Services:  make(map[string]*LBService, len(s.Services)),
	}
	for k, v := range s.Services {
		if v == nil {
			copyState.Services[k] = nil
			continue
		}
		svcCopy := &LBService{
			MACAddr:      v.MACAddr,
			Interface:    v.Interface,
			CurrentIP:    v.CurrentIP,
			DHCPAcquired: v.DHCPAcquired,
			Ports:        make([]PortEntry, len(v.Ports)),
		}
		for i, p := range v.Ports {
			svcCopy.Ports[i] = PortEntry{
				Protocol:     p.Protocol,
				ExternalPort: p.ExternalPort,
				PodPort:      p.PodPort,
				PodIPs:       append([]string(nil), p.PodIPs...),
			}
		}
		copyState.Services[k] = svcCopy
	}
	return copyState
}

func PortsEqual(a, b []PortEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !portEntryEqual(&a[i], &b[i]) {
			return false
		}
	}
	return true
}

func portEntryEqual(a, b *PortEntry) bool {
	if a.Protocol != b.Protocol {
		return false
	}
	if a.ExternalPort != b.ExternalPort {
		return false
	}
	if a.PodPort != b.PodPort {
		return false
	}
	if len(a.PodIPs) != len(b.PodIPs) {
		return false
	}
	for i := range a.PodIPs {
		if a.PodIPs[i] != b.PodIPs[i] {
			return false
		}
	}
	return true
}
