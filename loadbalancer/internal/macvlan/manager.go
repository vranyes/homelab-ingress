//go:build linux

package macvlan

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
)

type NetlinkManager struct {
	st     *state.State
	logger *slog.Logger
}

func NewNetlinkManagerWithLogger(st *state.State, logger *slog.Logger) *NetlinkManager {
	return &NetlinkManager{st: st, logger: logger}
}

func NewNetlinkManager() *NetlinkManager {
	return &NetlinkManager{logger: slog.Default()}
}

func (m *NetlinkManager) Create(iface, parent string, mac net.HardwareAddr) error {
	m.logger.Info("creating macvlan interface", "interface", iface, "parent", parent)

	parentLink, err := netlink.LinkByName(parent)
	if err != nil {
		m.logger.Error("failed to find parent interface", "parent", parent, "error", err)
		return fmt.Errorf("failed to find parent interface %q: %w", parent, err)
	}

	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:         iface,
			ParentIndex:  parentLink.Attrs().Index,
			HardwareAddr: mac,
		},
		Mode: netlink.MACVLAN_MODE_VEPA,
	}

	if err := netlink.LinkAdd(mv); err != nil {
		m.logger.Error("failed to create macvlan", "interface", iface, "error", err)
		return fmt.Errorf("failed to create macvlan %q: %w", iface, err)
	}

	m.logger.Info("macvlan interface created", "interface", iface)
	return nil
}

func (m *NetlinkManager) Delete(iface string) error {
	m.logger.Info("deleting macvlan interface", "interface", iface)

	link, err := netlink.LinkByName(iface)
	if err != nil {
		var lnf netlink.LinkNotFoundError
		if errors.As(err, &lnf) {
			m.logger.Debug("interface not found, skipping delete", "interface", iface)
			return nil
		}
		m.logger.Error("failed to find interface", "interface", iface, "error", err)
		return fmt.Errorf("failed to find interface %q: %w", iface, err)
	}

	if err := netlink.LinkDel(link); err != nil {
		m.logger.Error("failed to delete interface", "interface", iface, "error", err)
		return fmt.Errorf("failed to delete interface %q: %w", iface, err)
	}

	m.logger.Info("macvlan interface deleted", "interface", iface)
	return nil
}

func (m *NetlinkManager) Exists(iface string) bool {
	_, err := netlink.LinkByName(iface)
	if err != nil {
		m.logger.Debug("interface does not exist", "interface", iface)
		return false
	}
	m.logger.Debug("interface exists", "interface", iface)
	return true
}

func (m *NetlinkManager) GetMAC(iface string) (net.HardwareAddr, error) {
	m.logger.Debug("getting MAC address", "interface", iface)

	link, err := netlink.LinkByName(iface)
	if err != nil {
		m.logger.Error("failed to find interface", "interface", iface, "error", err)
		return nil, fmt.Errorf("failed to find interface %q: %w", iface, err)
	}
	mac := link.Attrs().HardwareAddr
	m.logger.Debug("MAC address retrieved", "interface", iface, "mac", mac)
	return mac, nil
}

func (m *NetlinkManager) SetUp(iface string) error {
	m.logger.Info("setting up interface", "interface", iface)

	link, err := netlink.LinkByName(iface)
	if err != nil {
		m.logger.Error("failed to find interface", "interface", iface, "error", err)
		return fmt.Errorf("failed to find interface %q: %w", iface, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		m.logger.Error("failed to set up interface", "interface", iface, "error", err)
		return fmt.Errorf("failed to set up interface %q: %w", iface, err)
	}

	m.logger.Info("interface is up", "interface", iface)
	return nil
}

func (m *NetlinkManager) SetIP(iface string, ip net.IP, subnetMask net.IPMask) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("failed to find interface: %w", err)
	}

	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: subnetMask,
		},
	}

	// Remove stale addresses and check if the new IP is already assigned.
	alreadyAssigned := false
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err == nil {
		for _, a := range addrs {
			if a.IP.Equal(ip) {
				m.logger.Debug("IP already assigned to interface", "interface", iface, "ip", ip)
				alreadyAssigned = true
			} else {
				m.logger.Info("removing stale IP from interface", "interface", iface, "stale_ip", a.IP)
				aCopy := a
				if rmErr := netlink.AddrDel(link, &aCopy); rmErr != nil {
					m.logger.Error("failed to remove stale IP", "interface", iface, "ip", a.IP, "error", rmErr)
				}
			}
		}
	}

	if alreadyAssigned {
		return nil
	}

	m.logger.Info("assigning IP to interface", "interface", iface, "ip", ip)
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("failed to add address: %w", err)
	}

	return nil
}

// Reconcile() looks at the current state and ensures that the actual interfaces match the desired state.
func (m *NetlinkManager) Reconcile() error {
	m.logger.Info("reconciling homelab load balancer interfaces")
	var e error

	return e
}

// Watch watches a channel managed by state package.
func (m *NetlinkManager) Run(svc <-chan state.LBNotification) {
	iface := "wan.201"
	for {
		select {
		case event := <-svc:
			name := InterfaceName(event.Service.ServiceName)
			switch event.Type {
			case state.CreateEvent:
				m.logger.Info("received create event", "service", event.Service)
				mac := GenerateMAC()
				if err := m.Create(name, iface, mac); err != nil {
					m.logger.Error("failed to create interface", "interface", name, "error", err)
				}
				event.Service.Interface = &name
				event.Service.MACAddr = mac
				if err := m.SetUp(name); err != nil {
					m.logger.Error("failed to set up interface", "interface", name, "error", err)
				}
				state.UpdateService(m.st, &event.Service)
			case state.UpdateEvent:
				m.logger.Info("received update event", "service", event.Service)
				m.logger.Info("setting IP address on interface", "interface", name, "ip", event.Service.SourceAddress)
				m.SetIP(name, event.Service.SourceAddress, event.Service.SourceMask)
			case state.DeleteEvent:
				m.logger.Info("received delete event", "service", event.Service)
				interfaceName := event.Service.GetInterfaceName()
				if err := m.Delete(interfaceName); err != nil {
					m.logger.Error("failed to delete interface", "interface", interfaceName, "error", err)
				}
			}
		case <-time.After(10 * time.Second):
			m.Reconcile()
		}
	}
}
