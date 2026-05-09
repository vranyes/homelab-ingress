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

func (m *NetlinkManager) Create(iface, parent string) error {
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
			HardwareAddr: GenerateMAC(),
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
	m.logger.Debug("MAC address retrieved", "interface", iface, "mac", mac.String())
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
			switch event.Type {
			case state.CreateEvent:
				m.logger.Info("received create event", "service", event.Service.ServiceName)
				if err := m.Create(event.Service.Interface, iface); err != nil {
					m.logger.Error("failed to create interface", "interface", event.Service.Interface, "error", err)
				}
			case state.UpdateEvent:
				m.logger.Info("received update event", "service", event.Service.ServiceName)
				// For now, we only care about IP changes, which don't require any action at the interface level.
			case state.DeleteEvent:
				m.logger.Info("received delete event", "service", event.Service.ServiceName)
				if err := m.Delete(event.Service.Interface); err != nil {
					m.logger.Error("failed to delete interface", "interface", event.Service.Interface, "error", err)
				}
			}
		case <-time.After(10 * time.Second):
			m.Reconcile()
		}
	}
}
