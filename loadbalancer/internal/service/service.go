package service

import (
	"context"
	"log/slog"
	"net"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
	routerv1 "github.com/vranyes/homelab-ingress/loadbalancer/pkg/api/router/v1"
)

type RouterServer struct {
	routerv1.UnimplementedRouterServiceServer
	store     *state.Store
	stateData *state.State
	logger    *slog.Logger
}

func NewRouterServer(store *state.Store, st *state.State) *RouterServer {
	return &RouterServer{
		store:     store,
		stateData: st,
		logger:    slog.Default(),
	}
}

func NewRouterServerWithLogger(store *state.Store, st *state.State, logger *slog.Logger) *RouterServer {
	return &RouterServer{
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

	ports := protoPortsToState(req.Ports)
	svc := &state.LBService{
		ServiceName: req.ServiceKey,
		Targets:     ports,
	}
	state.UpdateService(s.stateData, svc)

	s.logger.Info("load balancer created", "service_key", req.ServiceKey)
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

	svc.Targets = protoPortsToState(req.Ports)
	state.UpdateService(s.stateData, svc)

	s.logger.Info("load balancer updated", "service_key", req.ServiceKey)
	return &routerv1.UpdateLoadBalancerResponse{}, nil
}

func (s *RouterServer) DeleteLoadBalancer(ctx context.Context, req *routerv1.DeleteLoadBalancerRequest) (*routerv1.DeleteLoadBalancerResponse, error) {
	s.logger.Info("DeleteLoadBalancer request", "service_key", req.ServiceKey)

	if req.ServiceKey == "" {
		s.logger.Error("service_key is required")
		return nil, status.Error(codes.InvalidArgument, "service_key is required")
	}

	state.DeleteService(s.stateData, req.ServiceKey)

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

	s.logger.Debug("load balancer found", "service_key", req.ServiceKey)
	return &routerv1.GetLoadBalancerResponse{
		ServiceKey:    req.ServiceKey,
		AssignedIp:    svc.SourceAddress.String(),
		Ports:         statePortsToProto(svc.Targets),
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
			AssignedIp:    svc.SourceAddress.String(),
			Ports:         statePortsToProto(svc.Targets),
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
	s.logger.Debug("health check", "healthy", true, "active_macvlans", count)

	return &routerv1.HealthResponse{
		Healthy:        true,
		ActiveMacvlans: count,
	}, nil
}

func protoPortsToState(ports []*routerv1.PortMapping) []state.LBTarget {
	result := make([]state.LBTarget, len(ports))
	for i, p := range ports {
		result[i] = state.LBTarget{
			Protocol: p.Protocol,
			SrcPort:  int(p.SrcPort),
			Dst:      make([]state.LBDestination, len(p.GetDst())),
		}
		for j, addr := range p.GetDst() {
			result[i].Dst[j] = state.LBDestination{
				Port:    int(addr.GetPort()),
				Address: net.ParseIP(addr.GetAddr()),
			}
		}
	}
	return result
}

func statePortsToProto(ports []state.LBTarget) []*routerv1.PortMapping {
	result := make([]*routerv1.PortMapping, len(ports))
	for i, p := range ports {
		result[i] = &routerv1.PortMapping{
			Protocol: p.Protocol,
			SrcPort:  int32(p.SrcPort),
			Dst:      make([]*routerv1.Destination, len(p.Dst)),
		}
		for j, d := range p.Dst {
			result[i].Dst[j] = &routerv1.Destination{
				Port: int32(d.Port),
				Addr: d.Address.String(),
			}
		}
	}
	return result
}
