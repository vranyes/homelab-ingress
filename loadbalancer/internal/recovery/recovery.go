package recovery

import (
	"fmt"
	"log/slog"
	"net"

	"github.com/vranyes/homelab-ingress/loadbalancer/internal/dhcp"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/firewall"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
)

type LinkManager interface {
	Create(iface, parent string, mac net.HardwareAddr) error
	Delete(iface string) error
	Exists(iface string) bool
	GetMAC(iface string) (net.HardwareAddr, error)
	SetUp(iface string) error
}

type DHCPManager interface {
	NewAndAdd(iface string, mac net.HardwareAddr)
	Get(iface string) (dhcp.Client, bool)
	Remove(iface string)
	Start(iface string) error
}

type FirewallManager interface {
	AddServiceRules(iface, extIP string, ports []firewall.PortRule, serviceKey string) error
	RemoveRules(serviceKey string) error
}

func Run(logger *slog.Logger, st *state.State, linkMgr LinkManager, dhcpMgr DHCPManager, fwMgr FirewallManager) error {
	logger.Info("starting recovery", "services", len(st.Services))

	if len(st.Services) == 0 {
		logger.Info("no services to recover")
		return nil
	}

	for key, svc := range st.Services {
		logger.Info("recovering service", "service", key)
		if err := recoverService(logger, key, svc, linkMgr, dhcpMgr, fwMgr, st.BaseIface); err != nil {
			logger.Error("failed to recover service", "service", key, "error", err)
			return fmt.Errorf("recover service %s: %w", key, err)
		}
	}

	logger.Info("recovery complete", "services", len(st.Services))
	return nil
}

func recoverService(logger *slog.Logger, key string, svc *state.LBService, linkMgr LinkManager, dhcpMgr DHCPManager, fwMgr FirewallManager, parentIface string) error {
	logger.Info("recovering service", "service", key, "interface", svc.Interface, "current_ip", svc.CurrentIP)

	mac, err := svc.ParseMAC()
	if err != nil {
		logger.Error("failed to parse MAC", "service", key, "error", err)
		return fmt.Errorf("parse MAC for %s: %w", key, err)
	}
	logger.Info("MAC address parsed", "service", key, "mac", mac.String())

	if err := ensureInterface(logger, svc.Interface, parentIface, mac, linkMgr); err != nil {
		return err
	}

	ensureDHCP(logger, svc.Interface, mac, dhcpMgr)

	logger.Info("starting DHCP client for recovered service", "service", key, "interface", svc.Interface)
	if err := dhcpMgr.Start(svc.Interface); err != nil {
		logger.Error("failed to start DHCP", "service", key, "interface", svc.Interface, "error", err)
		return fmt.Errorf("start DHCP for %s: %w", svc.Interface, err)
	}

	if len(svc.Ports) > 0 {
		// Convert state.PortEntry -> firewall.PortRule and add all rules in one call
		ports := make([]firewall.PortRule, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			ports = append(ports, firewall.PortRule{ExtPort: p.ExternalPort, Protocol: p.Protocol, PodIPs: p.PodIPs, PodPort: p.PodPort})
		}
		logger.Info("recreating firewall rules for service", "service", key, "ports", len(ports))
		if err := fwMgr.AddServiceRules(svc.Interface, svc.CurrentIP, ports, key); err != nil {
			logger.Error("failed to recreate firewall rules during recovery", "service", key, "error", err)
		}
	}

	logger.Info("service recovery complete", "service", key)
	return nil
}

func ensureInterface(logger *slog.Logger, iface, parent string, mac net.HardwareAddr, linkMgr LinkManager) error {
	if linkMgr.Exists(iface) {
		existingMAC, err := linkMgr.GetMAC(iface)
		if err != nil {
			logger.Warn("could not read MAC from existing interface, will recreate", "interface", iface, "error", err)
		} else if existingMAC.String() != mac.String() {
			logger.Warn("MAC mismatch on existing interface, will recreate", "interface", iface, "expected", mac.String(), "got", existingMAC.String())
			if err := linkMgr.Delete(iface); err != nil {
				return fmt.Errorf("delete mismatched interface %s: %w", iface, err)
			}
		} else {
			logger.Info("interface already exists with correct MAC", "interface", iface, "mac", mac.String())
			return linkMgr.SetUp(iface)
		}
	}

	logger.Info("creating macvlan interface", "interface", iface, "parent", parent, "mac", mac.String())
	if err := linkMgr.Create(iface, parent, mac); err != nil {
		return fmt.Errorf("create macvlan %s: %w", iface, err)
	}
	if err := linkMgr.SetUp(iface); err != nil {
		return fmt.Errorf("set up macvlan %s: %w", iface, err)
	}
	return nil
}

func ensureDHCP(logger *slog.Logger, iface string, mac net.HardwareAddr, dhcpMgr DHCPManager) {
	logger.Info("ensuring DHCP client exists", "interface", iface)

	if _, ok := dhcpMgr.Get(iface); ok {
		logger.Info("DHCP client already attached", "interface", iface)
		return
	}
	logger.Info("attaching DHCP client", "interface", iface, "mac", mac.String())
	dhcpMgr.NewAndAdd(iface, mac)
}
