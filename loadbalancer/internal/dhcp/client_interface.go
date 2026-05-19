package dhcp

import (
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
)

type IPChangeFunc func(iface string, ip net.IP)

type Client interface {
	Start() error
	Stop()
	CurrentIP() net.IP
	IsAcquired() bool
}

type ClientFactory func(iface string, mac net.HardwareAddr, onChange IPChangeFunc) Client

type Manager struct {
	mu        sync.RWMutex
	clients   map[string]Client
	st        *state.State
	onChange  IPChangeFunc
	newClient ClientFactory
	logger    *slog.Logger
}

func NewManager(onChange IPChangeFunc) *Manager {
	return &Manager{
		clients:   make(map[string]Client),
		onChange:  onChange,
		newClient: nil,
		logger:    slog.Default(),
	}
}

func NewManagerWithLogger(onChange IPChangeFunc, logger *slog.Logger) *Manager {
	return &Manager{
		clients:   make(map[string]Client),
		onChange:  onChange,
		newClient: nil,
		logger:    logger,
	}
}

func NewManagerWithFactory(onChange IPChangeFunc, factory ClientFactory) *Manager {
	return &Manager{
		clients:   make(map[string]Client),
		onChange:  onChange,
		newClient: factory,
		logger:    slog.Default(),
	}
}

func NewManagerWithFactoryAndLogger(st *state.State, onChange IPChangeFunc, factory ClientFactory, logger *slog.Logger) *Manager {
	return &Manager{
		clients:   make(map[string]Client),
		st:        st,
		onChange:  onChange,
		newClient: factory,
		logger:    logger,
	}
}

func (m *Manager) Add(iface string, client Client) {
	m.logger.Info("adding DHCP client", "interface", iface)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[iface] = client
}

func (m *Manager) NewAndAdd(iface string, mac net.HardwareAddr) {
	if m.newClient == nil {
		m.logger.Warn("no client factory set, cannot create DHCP client", "interface", iface)
		return
	}
	m.logger.Info("creating new DHCP client", "interface", iface, "mac", mac.String())
	client := m.newClient(iface, mac, m.onChange)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[iface] = client
}

func (m *Manager) Start(iface string) error {
	m.logger.Info("starting DHCP client for interface", "interface", iface)
	m.mu.RLock()
	client, ok := m.clients[iface]
	m.mu.RUnlock()
	if !ok {
		m.logger.Error("DHCP client not found", "interface", iface)
		return fmt.Errorf("DHCP client not found for %s", iface)
	}
	return client.Start()
}

func (m *Manager) StartAll() error {
	m.logger.Info("starting all DHCP clients", "count", len(m.clients))
	m.mu.RLock()
	defer m.mu.RUnlock()
	var errs []error
	for iface, client := range m.clients {
		m.logger.Info("starting DHCP client", "interface", iface)
		if err := client.Start(); err != nil {
			m.logger.Error("failed to start DHCP client", "interface", iface, "error", err)
			errs = append(errs, fmt.Errorf("start %s: %w", iface, err))
		}
	}
	if len(errs) > 0 {
		m.logger.Error("failed to start some DHCP clients", "errors", len(errs))
		return fmt.Errorf("failed to start %d clients: %v", len(errs), errs)
	}
	return nil
}

func (m *Manager) Remove(iface string) {
	m.logger.Info("removing DHCP client", "interface", iface)
	m.mu.Lock()
	defer m.mu.Unlock()
	if client, ok := m.clients[iface]; ok {
		client.Stop()
		delete(m.clients, iface)
		m.logger.Info("DHCP client removed", "interface", iface)
	}
}

func (m *Manager) Get(iface string) (Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	client, ok := m.clients[iface]
	if !ok {
		m.logger.Debug("DHCP client not found", "interface", iface)
	}
	return client, ok
}

func (m *Manager) CurrentIP(iface string) (net.IP, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	client, ok := m.clients[iface]
	if !ok {
		m.logger.Debug("DHCP client not found", "interface", iface)
		return nil, false
	}
	ip := client.CurrentIP()
	if ip == nil {
		m.logger.Debug("no IP acquired yet", "interface", iface)
	}
	return ip, ip != nil
}

func (m *Manager) IsAcquired(iface string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	client, ok := m.clients[iface]
	if !ok {
		m.logger.Debug("DHCP client not found", "interface", iface)
		return false
	}
	return client.IsAcquired()
}

func (m *Manager) StopAll() {
	m.logger.Info("stopping all DHCP clients")
	m.mu.Lock()
	defer m.mu.Unlock()
	for iface, client := range m.clients {
		m.logger.Info("stopping DHCP client", "interface", iface)
		client.Stop()
		delete(m.clients, iface)
	}
	m.logger.Info("all DHCP clients stopped")
}

func (m *Manager) ListInterfaces() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	interfaces := make([]string, 0, len(m.clients))
	for iface := range m.clients {
		interfaces = append(interfaces, iface)
	}
	m.logger.Debug("listing DHCP interfaces", "count", len(interfaces))
	return interfaces
}

func (m *Manager) Run(svc <-chan state.LBNotification) {
	for {
		event := <-svc
		ifname := event.Service.GetInterfaceName()
		m.logger.Info("received service update", "interface", ifname, "event", event)
		switch event.Type {
		case state.CreateEvent, state.UpdateEvent:
			m.logger.Info("handling create/update event", "interface", ifname)
			for _, i := range m.ListInterfaces() {
				m.logger.Debug("checking existing interface", "interface", i)
				continue
			}
			m.NewAndAdd(ifname, event.Service.MACAddr)
		case state.DeleteEvent:
			// Handle delete event
			m.logger.Info("handling delete event", "interface", ifname)
			m.Remove(ifname)
		}
	}
}
