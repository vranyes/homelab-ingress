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
	UpdateCh  chan LBNotification   `json:"-"`
}

type LBEvent string

const CreateEvent LBEvent = "create"
const UpdateEvent LBEvent = "update"
const DeleteEvent LBEvent = "delete"

type LBNotification struct {
	Type    LBEvent
	Service LBService
}

type LBService struct {
	ServiceName   string           `json:"service_name"`
	Interface     *string          `json:"interface"`
	MACAddr       net.HardwareAddr `json:"macaddr"`
	DHCPAcquired  *bool            `json:"dhcp_acquired"`
	SourceAddress net.IP           `json:"source_address"`
	SourceMask    net.IPMask       `json:"source_mask,omitempty"`
	Targets       []LBTarget       `json:"targets"`
}

func (s *LBService) String() string {
	return s.ServiceName
}

func (s *LBService) Clone() *LBService {
	if s == nil {
		return nil
	}
	copy := *s
	if s.Interface != nil {
		ifaceCopy := *s.Interface
		copy.Interface = &ifaceCopy
	}
	if s.DHCPAcquired != nil {
		dhcpCopy := *s.DHCPAcquired
		copy.DHCPAcquired = &dhcpCopy
	}
	copy.Targets = make([]LBTarget, len(s.Targets))
	for i, t := range s.Targets {
		copy.Targets[i] = LBTarget{
			Protocol: t.Protocol,
			SrcPort:  t.SrcPort,
			Dst:      make([]LBDestination, len(t.Dst)),
		}
		for j, d := range t.Dst {
			copy.Targets[i].Dst[j] = LBDestination{
				Port:    d.Port,
				Address: d.Address,
			}
		}
	}
	return &copy
}

func (s *LBService) GetInterfaceName() string {
	if s.Interface == nil {
		return ""
	}
	return *s.Interface
}

func (s *LBService) SetInterfaceName(name string) {
	s.Interface = &name
}

type LBTarget struct {
	Protocol string          `json:"protocol"`
	SrcPort  int             `json:"external_port"`
	Dst      []LBDestination `json:"destinations"`
}

type LBDestination struct {
	Port    int    `json:"port"`
	Address net.IP `json:"address,omitempty"`
}

func NewState(baseIface string) *State {
	return &State{
		Version:   StateVersion,
		BaseIface: baseIface,
		Services:  make(map[string]*LBService),
		UpdateCh:  make(chan LBNotification),
	}
}

func (s *LBService) ParseMAC() (net.HardwareAddr, error) {
	return s.MACAddr, nil
}

func (s *LBService) SetMAC(mac net.HardwareAddr) {
	s.MACAddr = mac
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
			Interface:     v.Interface,
			MACAddr:       v.MACAddr,
			DHCPAcquired:  v.DHCPAcquired,
			SourceAddress: v.SourceAddress,
			Targets:       make([]LBTarget, len(v.Targets)),
		}
		for i, p := range v.Targets {
			svcCopy.Targets[i] = LBTarget{
				Protocol: p.Protocol,
				SrcPort:  p.SrcPort,
				Dst:      make([]LBDestination, len(p.Dst)),
			}
			for j, d := range p.Dst {
				svcCopy.Targets[i].Dst[j] = LBDestination{
					Port:    d.Port,
					Address: d.Address,
				}
			}
		}
		copyState.Services[k] = svcCopy
	}
	return copyState
}
