package service

import (
	"context"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vranyes/homelab-ingress/loadbalancer/internal/dhcp"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/firewall"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/macvlan"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
	routerv1 "github.com/vranyes/homelab-ingress/loadbalancer/pkg/api/router/v1"
)

// firewallManager is the subset of firewall.Manager used by RouterServer.
type firewallManager interface {
	AddServiceRules(iface, extIP string, ports []firewall.PortRule, serviceKey string) error
	RemoveRules(serviceKey string) error
}

type RouterServer struct {
	routerv1.UnimplementedRouterServiceServer
	baseIface string
	linkMgr   macvlan.LinkManager
	dhcpMgr   *dhcp.Manager
	fwMgr     firewallManager
	store     *state.Store
	stateData *state.State
	logger    *slog.Logger
}

func NewRouterServer(baseIface string, linkMgr macvlan.LinkManager, dhcpMgr *dhcp.Manager, fwMgr firewallManager, store *state.Store, st *state.State) *RouterServer {
	return &RouterServer{
		baseIface: baseIface,
		linkMgr:   linkMgr,
		dhcpMgr:   dhcpMgr,
		fwMgr:     fwMgr,
		store:     store,
		stateData: st,
		logger:    slog.Default(),
	}
}

func NewRouterServerWithLogger(baseIface string, linkMgr macvlan.LinkManager, dhcpMgr *dhcp.Manager, fwMgr firewallManager, store *state.Store, st *state.State, logger *slog.Logger) *RouterServer {
	return &RouterServer{
		baseIface: baseIface,
		linkMgr:   linkMgr,
		dhcpMgr:   dhcpMgr,
		fwMgr:     fwMgr,
		store:     store,
		stateData: st,
		logger:    logger,
	}
}

func (s *RouterServer) CreateLoadBalancer(ctx context.Context, req *routerv1.CreateLoadBalancerRequest) (*routerv1.CreateLoadBalancerResponse, error) {
	s.logger.Info("CreateLoadBalancer request", "service_key", req.ServiceKey, "ports", len(req.Ports))

	if req.ServiceKey == "" {
		s.logger.Error("service_key is required")
		return nil, status.Error(codes.InvalidArgument, "service_key is required")
	}
	if len(req.Ports) == 0 {
		s.logger.Error("at least one port mapping is required", "service_key", req.ServiceKey)
		return nil, status.Error(codes.InvalidArgument, "at least one port mapping is required")
	}

	if state.HasService(s.stateData, req.ServiceKey) {
		s.logger.Warn("service already exists", "service_key", req.ServiceKey)
		return nil, status.Error(codes.AlreadyExists, "service already exists")
	}

	iface := macvlan.InterfaceName(req.ServiceKey)
	if s.linkMgr.Exists(iface) {
		s.logger.Error("interface already exists", "interface", iface)
		return nil, status.Errorf(codes.AlreadyExists, "interface %s already exists", iface)
	}

	mac := macvlan.GenerateMAC()
	s.logger.Info("creating load balancer", "service_key", req.ServiceKey, "interface", iface, "mac", mac.String())

	if err := s.linkMgr.Create(iface, s.baseIface, mac); err != nil {
		s.logger.Error("failed to create macvlan", "interface", iface, "error", err)
		return nil, status.Errorf(codes.Internal, "failed to create macvlan: %v", err)
	}
	if err := s.linkMgr.SetUp(iface); err != nil {
		s.logger.Error("failed to bring up interface", "interface", iface, "error", err)
		return nil, status.Errorf(codes.Internal, "failed to bring up interface: %v", err)
	}

	ports := protoPortsToState(req.Ports)
	state.UpdateService(s.stateData, req.ServiceKey, mac, iface, ports)
	if err := s.store.Save(s.stateData); err != nil {
		s.logger.Error("failed to save state", "error", err)
		return nil, status.Errorf(codes.Internal, "failed to save state: %v", err)
	}

	s.logger.Info("attaching DHCP client", "interface", iface, "service_key", req.ServiceKey)
	s.dhcpMgr.NewAndAdd(iface, mac)
	client, _ := s.dhcpMgr.Get(iface)
	if client != nil {
		s.logger.Info("starting DHCP client", "interface", iface)
		if err := client.Start(); err != nil {
			s.logger.Error("failed to start DHCP", "interface", iface, "error", err)
			return nil, status.Errorf(codes.Internal, "failed to start DHCP: %v", err)
		}
	}

	s.logger.Info("load balancer created", "service_key", req.ServiceKey, "interface", iface)
	return &routerv1.CreateLoadBalancerResponse{}, nil
}

func (s *RouterServer) UpdateLoadBalancer(ctx context.Context, req *routerv1.UpdateLoadBalancerRequest) (*routerv1.UpdateLoadBalancerResponse, error) {
	s.logger.Info("UpdateLoadBalancer request", "service_key", req.ServiceKey, "ports", len(req.Ports))

	if req.ServiceKey == "" {
		s.logger.Error("service_key is required")
		return nil, status.Error(codes.InvalidArgument, "service_key is required")
	}

	svc, ok := state.GetService(s.stateData, req.ServiceKey)
	if !ok {
		s.logger.Error("service not found", "service_key", req.ServiceKey)
		return nil, status.Error(codes.NotFound, "service not found")
	}

	iface := svc.Interface
	ports := protoPortsToState(req.Ports)

	s.logger.Info("updating firewall rules", "service_key", req.ServiceKey, "interface", iface)
	fwPorts := make([]firewall.PortRule, len(ports))
	for i, port := range ports {
		fwPorts[i] = firewall.PortRule{
			ExtPort:  port.ExternalPort,
			Protocol: port.Protocol,
			PodIPs:   port.PodIPs,
			PodPort:  port.PodPort,
		}
	}
	// Only apply firewall rules if the service has an assigned external IP.
	// Applying firewall rules without a matching external IP could allow
	// unintended traffic to be forwarded to pod IPs.
	if err := s.fwMgr.AddServiceRules(iface, svc.CurrentIP, fwPorts, req.ServiceKey); err != nil {
		s.logger.Error("failed to add firewall rules", "service_key", req.ServiceKey, "error", err)
		return nil, status.Errorf(codes.Internal, "failed to add firewall rules: %v", err)
	}

	// Update ports under state package lock to avoid races with concurrent
	// DHCP updates and other handlers.
	// Preserve existing MAC and interface values when updating only ports.
	// To avoid nil MAC being passed to UpdateService (can cause panics), capture
	// the original service MAC and use it; if parsing fails, generate a new MAC.
	existingMAC, err := svc.ParseMAC()
	if err != nil || len(existingMAC) == 0 {
		s.logger.Warn("existing service has invalid MAC, generating new MAC", "service_key", req.ServiceKey, "error", err)
		existingMAC = macvlan.GenerateMAC()
	}
	state.UpdateService(s.stateData, req.ServiceKey, existingMAC, iface, ports)

	if err := s.store.Save(s.stateData); err != nil {
		s.logger.Error("failed to save state", "error", err)
		return nil, status.Errorf(codes.Internal, "failed to save state: %v", err)
	}

	s.logger.Info("load balancer updated", "service_key", req.ServiceKey)
	return &routerv1.UpdateLoadBalancerResponse{}, nil
}

func (s *RouterServer) DeleteLoadBalancer(ctx context.Context, req *routerv1.DeleteLoadBalancerRequest) (*routerv1.DeleteLoadBalancerResponse, error) {
	s.logger.Info("DeleteLoadBalancer request", "service_key", req.ServiceKey)

	if req.ServiceKey == "" {
		s.logger.Error("service_key is required")
		return nil, status.Error(codes.InvalidArgument, "service_key is required")
	}

	svc, ok := state.GetService(s.stateData, req.ServiceKey)
	if !ok {
		s.logger.Debug("service not found, nothing to delete", "service_key", req.ServiceKey)
		return &routerv1.DeleteLoadBalancerResponse{}, nil
	}

	s.logger.Info("removing DHCP client", "service_key", req.ServiceKey, "interface", svc.Interface)
	s.dhcpMgr.Remove(svc.Interface)

	s.logger.Info("removing firewall rules", "service_key", req.ServiceKey)
	if err := s.fwMgr.RemoveRules(req.ServiceKey); err != nil {
		s.logger.Error("failed to remove firewall rules", "service_key", req.ServiceKey, "error", err)
		return nil, status.Errorf(codes.Internal, "failed to remove firewall rules: %v", err)
	}

	s.logger.Info("deleting interface", "service_key", req.ServiceKey, "interface", svc.Interface)
	if err := s.linkMgr.Delete(svc.Interface); err != nil {
		s.logger.Error("failed to delete interface", "interface", svc.Interface, "error", err)
		return nil, status.Errorf(codes.Internal, "failed to delete interface: %v", err)
	}

	state.DeleteService(s.stateData, req.ServiceKey)
	if err := s.store.Save(s.stateData); err != nil {
		s.logger.Error("failed to save state", "error", err)
		return nil, status.Errorf(codes.Internal, "failed to save state: %v", err)
	}

	s.logger.Info("load balancer deleted", "service_key", req.ServiceKey)
	return &routerv1.DeleteLoadBalancerResponse{}, nil
}

func (s *RouterServer) GetLoadBalancer(ctx context.Context, req *routerv1.GetLoadBalancerRequest) (*routerv1.GetLoadBalancerResponse, error) {
	s.logger.Debug("GetLoadBalancer request", "service_key", req.ServiceKey)

	if req.ServiceKey == "" {
		s.logger.Error("service_key is required")
		return nil, status.Error(codes.InvalidArgument, "service_key is required")
	}

	svc, ok := state.GetService(s.stateData, req.ServiceKey)
	if !ok {
		s.logger.Error("service not found", "service_key", req.ServiceKey)
		return nil, status.Error(codes.NotFound, "service not found")
	}

	s.logger.Debug("load balancer found", "service_key", req.ServiceKey, "ip", svc.CurrentIP, "interface", svc.Interface)
	return &routerv1.GetLoadBalancerResponse{
		ServiceKey:    req.ServiceKey,
		AssignedIp:    svc.CurrentIP,
		Ports:         statePortsToProto(svc.Ports),
		DhcpAcquired:  svc.DHCPAcquired,
		InterfaceName: svc.Interface,
	}, nil
}

func (s *RouterServer) ListLoadBalancers(ctx context.Context, req *routerv1.ListLoadBalancersRequest) (*routerv1.ListLoadBalancersResponse, error) {
	s.logger.Debug("ListLoadBalancers request")

	// Snapshot state to iterate safely without holding locks.
	snap := s.stateData.Clone()

	var lbs []*routerv1.GetLoadBalancerResponse
	for key, svc := range snap.Services {
		lbs = append(lbs, &routerv1.GetLoadBalancerResponse{
			ServiceKey:    key,
			AssignedIp:    svc.CurrentIP,
			Ports:         statePortsToProto(svc.Ports),
			DhcpAcquired:  svc.DHCPAcquired,
			InterfaceName: svc.Interface,
		})
	}

	s.logger.Debug("load balancers listed", "count", len(lbs))
	return &routerv1.ListLoadBalancersResponse{LoadBalancers: lbs}, nil
}

func (s *RouterServer) Health(ctx context.Context, req *routerv1.HealthRequest) (*routerv1.HealthResponse, error) {
	s.logger.Debug("Health check request")

	// Snapshot state to compute accurate counts without racing.
	snap := s.stateData.Clone()
	count := int32(len(snap.Services))
	s.logger.Debug("health check", "healthy", true, "base_interface", s.baseIface, "active_macvlans", count)

	return &routerv1.HealthResponse{
		Healthy:        true,
		BaseInterface:  s.baseIface,
		ActiveMacvlans: count,
	}, nil
}

func protoPortsToState(ports []*routerv1.PortMapping) []state.PortEntry {
	result := make([]state.PortEntry, len(ports))
	for i, p := range ports {
		result[i] = state.PortEntry{
			Protocol:     p.Protocol,
			ExternalPort: int(p.ExternalPort),
			PodPort:      int(p.PodPort),
			PodIPs:       p.PodIps,
		}
	}
	return result
}

func statePortsToProto(ports []state.PortEntry) []*routerv1.PortMapping {
	result := make([]*routerv1.PortMapping, len(ports))
	for i, p := range ports {
		result[i] = &routerv1.PortMapping{
			Protocol:     p.Protocol,
			ExternalPort: int32(p.ExternalPort),
			PodPort:      int32(p.PodPort),
			PodIps:       p.PodIPs,
		}
	}
	return result
}
