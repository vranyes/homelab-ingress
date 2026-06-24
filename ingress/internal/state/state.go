package state

import (
	"log/slog"
	"net"
	"sync"
)

const StateVersion = 1

type State struct {
	mu          sync.RWMutex
	Version     int                   `json:"version"`
	BaseIface   string                `json:"base_interface"`
	Services    map[string]*LBService `json:"services"`
	subscribers []chan LBNotification `json:"-"`
	store       *Store
	logger      *slog.Logger
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
	cpy := *s
	if s.Interface != nil {
		ifaceCpy := *s.Interface
		cpy.Interface = &ifaceCpy
	}
	if s.DHCPAcquired != nil {
		dhcpCpy := *s.DHCPAcquired
		cpy.DHCPAcquired = &dhcpCpy
	}
	if s.MACAddr != nil {
		cpy.MACAddr = make(net.HardwareAddr, len(s.MACAddr))
		copy(cpy.MACAddr, s.MACAddr)
	}
	if s.SourceAddress != nil {
		cpy.SourceAddress = make(net.IP, len(s.SourceAddress))
		copy(cpy.SourceAddress, s.SourceAddress)
	}
	if s.SourceMask != nil {
		cpy.SourceMask = make(net.IPMask, len(s.SourceMask))
		copy(cpy.SourceMask, s.SourceMask)
	}
	cpy.Targets = make([]LBTarget, len(s.Targets))
	for i, t := range s.Targets {
		cpy.Targets[i] = LBTarget{
			Protocol: t.Protocol,
			SrcPort:  t.SrcPort,
			Dst:      make([]LBDestination, len(t.Dst)),
		}
		for j, d := range t.Dst {
			cpy.Targets[i].Dst[j] = LBDestination{
				Port: d.Port,
			}
			if d.Address != nil {
				cpy.Targets[i].Dst[j].Address = make(net.IP, len(d.Address))
				copy(cpy.Targets[i].Dst[j].Address, d.Address)
			}
		}
	}
	return &cpy
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
	SrcPort  uint16          `json:"external_port"`
	Dst      []LBDestination `json:"destinations"`
}

type LBDestination struct {
	Port    uint16 `json:"port"`
	Address net.IP `json:"address,omitempty"`
}

func NewState(baseIface string, store *Store, logger *slog.Logger) *State {
	return &State{
		Version:     StateVersion,
		BaseIface:   baseIface,
		Services:    make(map[string]*LBService),
		subscribers: make([]chan LBNotification, 0),
		store:       store,
		logger:      logger,
	}
}

func (s *State) Subscribe() chan LBNotification {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan LBNotification, 64)
	s.subscribers = append(s.subscribers, ch)
	return ch
}

func (s *State) notify(notification LBNotification) {
	s.mu.RLock()
	subs := s.subscribers
	s.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- notification:
		default:
		}
	}
}

func (s *LBService) ParseMAC() (net.HardwareAddr, error) {
	return s.MACAddr, nil
}

func (s *LBService) SetMAC(mac net.HardwareAddr) {
	s.MACAddr = mac
}

func (s *State) UpdateService(svc *LBService) {
	s.mu.Lock()
	eventType := UpdateEvent
	_, ok := s.Services[svc.ServiceName]
	if !ok {
		eventType = CreateEvent
	}
	s.Services[svc.ServiceName] = svc
	s.mu.Unlock()

	if s.store != nil {
		if err := s.store.SaveService(svc); err != nil {
			svcClone := svc.Clone()
			svcClone.ServiceName = svc.ServiceName + " (save failed)"
			s.logger.Error("failed to persist service", "service", svc.ServiceName, "error", err)
		}
	}

	s.notify(LBNotification{
		Type:    eventType,
		Service: *svc.Clone(),
	})
}

func (s *State) DeleteService(serviceKey string) {
	s.mu.Lock()
	svc, ok := s.Services[serviceKey]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.Services, serviceKey)
	s.mu.Unlock()

	if s.store != nil {
		if err := s.store.DeleteService(serviceKey); err != nil {
			s.logger.Error("failed to delete persisted service", "service", serviceKey, "error", err)
		}
	}

	s.notify(LBNotification{
		Type:    DeleteEvent,
		Service: *svc.Clone(),
	})
}

func (s *State) HasService(serviceKey string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.Services[serviceKey]
	return ok
}

func (s *State) GetService(serviceKey string) (*LBService, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	svc, ok := s.Services[serviceKey]
	return svc, ok
}

func (s *State) Clone() *State {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cpy := &State{
		Version:   s.Version,
		BaseIface: s.BaseIface,
		Services:  make(map[string]*LBService, len(s.Services)),
	}
	for k, v := range s.Services {
		if v == nil {
			cpy.Services[k] = nil
			continue
		}
		cpy.Services[k] = v.Clone()
	}
	return cpy
}

func (s *State) Close() error {
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}
