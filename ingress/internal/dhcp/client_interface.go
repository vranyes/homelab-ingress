//go:build linux

package dhcp

import (
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

const reconcileInterval = 30 * time.Second

type IPChangeFunc func(iface string, ip net.IP, mask net.IPMask)

type Client interface {
	Start() error
	Stop()
	CurrentIP() net.IP
	IsAcquired() bool
}

type Manager struct {
	mu             sync.RWMutex
	clients        map[string]Client
	ifaceToService map[string]string
	st             *state.State
	logger         *slog.Logger
	done           chan struct{}
	stopOnce       sync.Once
}

func NewManager(st *state.State, logger *slog.Logger) *Manager {
	return &Manager{
		clients:        make(map[string]Client),
		ifaceToService: make(map[string]string),
		st:             st,
		logger:         logger,
		done:           make(chan struct{}),
	}
}

func (m *Manager) ensureAndStart(iface string, mac net.HardwareAddr) {
	m.mu.Lock()
	client, exists := m.clients[iface]
	if !exists {
		m.logger.Info("creating new DHCP client", "interface", iface, "mac", mac.String())
		client = newRealClient(iface, mac, m.onIPChange, m.logger)
		m.clients[iface] = client
	}
	m.mu.Unlock()

	if err := client.Start(); err != nil {
		m.logger.Error("failed to start DHCP client", "interface", iface, "error", err)
	}
}

func (m *Manager) onIPChange(iface string, ip net.IP, mask net.IPMask) {
	key := m.serviceKeyForInterface(iface)
	if key == "" {
		m.logger.Warn("could not find service key for interface", "interface", iface)
		return
	}

	m.logger.Info("DHCP IP change", "interface", iface, "ip", ip.String(), "mask", mask, "service_key", key)

	svc, ok := m.st.GetService(key)
	if !ok {
		m.logger.Warn("service disappeared during IP change", "service_key", key)
		return
	}

	clone := svc.Clone()
	clone.SourceAddress = ip
	clone.SourceMask = mask
	acquired := true
	clone.DHCPAcquired = &acquired
	m.st.UpdateService(clone)
}

func (m *Manager) serviceKeyForInterface(iface string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ifaceToService[iface]
}

func (m *Manager) setServiceMapping(serviceKey, iface string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ifaceToService[iface] = serviceKey
}

func (m *Manager) deleteServiceMapping(serviceKey string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for iface, key := range m.ifaceToService {
		if key == serviceKey {
			delete(m.ifaceToService, iface)
			return iface
		}
	}
	return ""
}

func (m *Manager) remove(iface string) {
	m.logger.Info("removing DHCP client", "interface", iface)
	m.mu.Lock()
	client, ok := m.clients[iface]
	if ok {
		delete(m.clients, iface)
	}
	m.mu.Unlock()

	if ok {
		client.Stop()
		m.logger.Info("DHCP client removed", "interface", iface)
	}
}

func (m *Manager) StopAll() {
	m.logger.Info("stopping all DHCP clients")
	m.mu.Lock()
	clientsCopy := make(map[string]Client, len(m.clients))
	for iface, client := range m.clients {
		clientsCopy[iface] = client
	}
	m.clients = make(map[string]Client)
	m.ifaceToService = make(map[string]string)
	m.mu.Unlock()

	for iface, client := range clientsCopy {
		m.logger.Info("stopping DHCP client", "interface", iface)
		client.Stop()
	}
	m.logger.Info("all DHCP clients stopped")
}

func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		close(m.done)
	})
}

func (m *Manager) Run(svc <-chan state.LBNotification) {
	reconcile := time.NewTicker(reconcileInterval)
	defer reconcile.Stop()

	for {
		select {
		case <-m.done:
			m.logger.Info("DHCP manager shutting down")
			return

		case event, ok := <-svc:
			if !ok {
				m.logger.Warn("notification channel closed, shutting down")
				return
			}
			if event.Service.ServiceName == "" {
				m.logger.Debug("skipping empty event")
				continue
			}

			ifname := event.Service.GetInterfaceName()
			m.logger.Info("received service update", "interface", ifname, "event", event.Type, "service", event.Service.ServiceName)
			switch event.Type {
			case state.CreateEvent, state.UpdateEvent:
				m.handleUpsert(ifname, event.Service)
			case state.DeleteEvent:
				m.handleDelete(ifname, event.Service.ServiceName)
			}

		case <-reconcile.C:
			m.reconcile()
		}
	}
}

func (m *Manager) handleUpsert(ifname string, svc state.LBService) {
	if ifname == "" {
		m.logger.Debug("skipping event, no interface name", "service", svc.ServiceName)
		return
	}
	if svc.MACAddr == nil {
		m.logger.Debug("skipping event, MAC address not yet assigned", "service", svc.ServiceName)
		return
	}

	oldIface := m.serviceKeyToIface(svc.ServiceName)
	if oldIface != "" && oldIface != ifname {
		m.logger.Info("interface name changed, removing old DHCP client", "service", svc.ServiceName, "old_interface", oldIface, "new_interface", ifname)
		m.remove(oldIface)
	}

	m.setServiceMapping(svc.ServiceName, ifname)
	m.ensureAndStart(ifname, svc.MACAddr)
}

func (m *Manager) serviceKeyToIface(serviceKey string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for iface, key := range m.ifaceToService {
		if key == serviceKey {
			return iface
		}
	}
	return ""
}

func (m *Manager) handleDelete(ifname string, serviceKey string) {
	if ifname == "" {
		ifname = m.deleteServiceMapping(serviceKey)
	} else {
		m.deleteServiceMapping(serviceKey)
	}

	if ifname == "" {
		m.logger.Debug("no interface name for deleted service, cannot remove DHCP client", "service", serviceKey)
		return
	}
	m.remove(ifname)
}

func (m *Manager) reconcile() {
	m.logger.Debug("reconciling DHCP clients")

	snap := m.st.Clone()

	m.mu.RLock()
	activeIfaces := make(map[string]bool, len(m.clients))
	for iface := range m.clients {
		activeIfaces[iface] = true
	}
	m.mu.RUnlock()

	type desiredClient struct {
		iface      string
		mac        net.HardwareAddr
		serviceKey string
	}
	desired := make(map[string]desiredClient)
	for key, svc := range snap.Services {
		ifname := svc.GetInterfaceName()
		if ifname == "" || svc.MACAddr == nil {
			continue
		}
		if existing, dup := desired[ifname]; dup {
			m.logger.Warn("duplicate interface name", "interface", ifname, "service_key_1", existing.serviceKey, "service_key_2", key)
		}
		desired[ifname] = desiredClient{iface: ifname, mac: svc.MACAddr, serviceKey: key}
	}

	for iface, d := range desired {
		if !activeIfaces[iface] {
			m.logger.Info("reconcile: creating missing DHCP client", "interface", iface)
			m.setServiceMapping(d.serviceKey, d.iface)
			m.ensureAndStart(d.iface, d.mac)
		}
	}

	for iface := range activeIfaces {
		if _, wanted := desired[iface]; !wanted {
			m.logger.Info("reconcile: removing stale DHCP client", "interface", iface)
			m.remove(iface)
		}
	}
}
