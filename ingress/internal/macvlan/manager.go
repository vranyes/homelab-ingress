//go:build linux

package macvlan

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

const reconcileInterval = 30 * time.Second

type NetlinkManager struct {
	st     *state.State
	logger *slog.Logger
}

func NewNetlinkManager(st *state.State, logger *slog.Logger) *NetlinkManager {
	return &NetlinkManager{st: st, logger: logger}
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

func (m *NetlinkManager) SetMAC(iface string, mac net.HardwareAddr) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("failed to find interface: %w", err)
	}
	if link.Attrs().HardwareAddr.String() == mac.String() {
		return nil
	}
	m.logger.Info("setting MAC address on interface", "interface", iface, "mac", mac)
	if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
		return fmt.Errorf("failed to set MAC address: %w", err)
	}
	return nil
}

func (m *NetlinkManager) SetIP(iface string, ip net.IP, subnetMask net.IPMask) error {
	if ip == nil || subnetMask == nil {
		return nil
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("failed to find interface: %w", err)
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("failed to list addresses: %w", err)
	}

	for _, a := range addrs {
		if a.IP.Equal(ip) {
			if bytes.Equal(a.Mask, subnetMask) {
				m.logger.Debug("IP already assigned with correct mask", "interface", iface, "ip", ip)
				return nil
			}
			m.logger.Info("IP mask mismatch, removing stale address", "interface", iface, "ip", ip)
			aCopy := a
			if rmErr := netlink.AddrDel(link, &aCopy); rmErr != nil {
				m.logger.Error("failed to remove mismatched IP", "interface", iface, "ip", ip, "error", rmErr)
			}
		} else {
			m.logger.Info("removing address not managed by this service", "interface", iface, "ip", a.IP)
			aCopy := a
			if rmErr := netlink.AddrDel(link, &aCopy); rmErr != nil {
				m.logger.Error("failed to remove address", "interface", iface, "ip", a.IP, "error", rmErr)
			}
		}
	}

	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: subnetMask,
		},
	}

	m.logger.Info("assigning IP to interface", "interface", iface, "ip", ip)
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("failed to add address: %w", err)
	}

	return nil
}

func (m *NetlinkManager) Run(svc <-chan state.LBNotification) {
	iface := m.st.BaseIface
	reconcile := time.NewTicker(reconcileInterval)
	defer reconcile.Stop()

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
					break
				}
				event.Service.Interface = &name
				event.Service.MACAddr = mac
				if err := m.SetUp(name); err != nil {
					m.logger.Error("failed to set up interface", "interface", name, "error", err)
				}
				m.st.UpdateService(&event.Service)
			case state.UpdateEvent:
				m.logger.Info("received update event", "service", event.Service)
				if event.Service.SourceAddress != nil && event.Service.SourceMask != nil {
					m.logger.Info("setting IP address on interface", "interface", name, "ip", event.Service.SourceAddress)
					if err := m.SetIP(name, event.Service.SourceAddress, event.Service.SourceMask); err != nil {
						m.logger.Error("failed to set IP address", "interface", name, "error", err)
					}
				}
				if len(event.Service.MACAddr) > 0 {
					if err := m.SetMAC(name, event.Service.MACAddr); err != nil {
						m.logger.Error("failed to set MAC address", "interface", name, "error", err)
					}
				}
			case state.DeleteEvent:
				m.logger.Info("received delete event", "service", event.Service)
				if err := m.Delete(name); err != nil {
					m.logger.Error("failed to delete interface", "interface", name, "error", err)
				}
			}

		case <-reconcile.C:
			m.reconcile()
		}
	}
}

func (m *NetlinkManager) ActiveCount() int32 {
	links, err := netlink.LinkList()
	if err != nil {
		m.logger.Error("failed to list links", "error", err)
		return 0
	}
	var count int32
	for _, link := range links {
		if strings.HasPrefix(link.Attrs().Name, "lb-") {
			count++
		}
	}
	return count
}

func (m *NetlinkManager) reconcile() {
	m.logger.Debug("reconciling macvlan interfaces")

	snap := m.st.Clone()

	for _, svc := range snap.Services {
		ifname := svc.GetInterfaceName()
		if ifname == "" {
			continue
		}

		exists := m.Exists(ifname)

		if !exists {
			m.logger.Info("reconcile: recreating missing macvlan interface", "interface", ifname, "service", svc.ServiceName)

			mac := svc.MACAddr
			if mac == nil {
				mac = GenerateMAC()
			}

			if err := m.Create(ifname, m.st.BaseIface, mac); err != nil {
				m.logger.Error("reconcile: failed to recreate interface", "interface", ifname, "error", err)
				continue
			}

			svc.Interface = &ifname
			svc.MACAddr = mac

			if err := m.SetUp(ifname); err != nil {
				m.logger.Error("reconcile: failed to set up interface", "interface", ifname, "error", err)
			}

			m.st.UpdateService(svc)
		}

		if svc.SourceAddress != nil && svc.SourceMask != nil {
			if err := m.SetIP(ifname, svc.SourceAddress, svc.SourceMask); err != nil {
				m.logger.Error("reconcile: failed to set IP", "interface", ifname, "ip", svc.SourceAddress, "error", err)
			}
		}

		if len(svc.MACAddr) > 0 {
			if err := m.SetMAC(ifname, svc.MACAddr); err != nil {
				m.logger.Error("reconcile: failed to set MAC", "interface", ifname, "mac", svc.MACAddr, "error", err)
			}
		}
	}
}
