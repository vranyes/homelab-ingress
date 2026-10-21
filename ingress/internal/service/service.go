package service

import (
	"context"
	"log/slog"
	"net"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
	routerv1 "github.com/vranyes/homelab-ingress/ingress/pkg/api/router/v1"
)

var isoAlpha2RE = regexp.MustCompile(`^[A-Z]{2}$`)

type RouterServer struct {
	routerv1.UnimplementedRouterServiceServer
	stateData         *state.State
	logger            *slog.Logger
	ActiveMacvlanFunc func() int32
	// GeoEnabled is set when a country database is configured (MaxMind,
	// DB-IP or the e2e mock). Requests that carry allowed_geos are rejected
	// while it is false (ADR 0003, ADR 0006).
	GeoEnabled bool
}

func NewRouterServer(st *state.State, logger *slog.Logger) *RouterServer {
	return &RouterServer{
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
	if s.stateData.HasService(req.ServiceKey) {
		s.logger.Warn("service already exists", "service_key", req.ServiceKey)
		return nil, status.Error(codes.AlreadyExists, "service already exists")
	}

	geos, err := s.geosFromRequest(req.AllowedGeos)
	if err != nil {
		return nil, err
	}

	ports := protoPortsToState(req.Ports)
	svc := &state.LBService{
		ServiceName: req.ServiceKey,
		Targets:     ports,
		AllowedGeos: geos,
	}
	s.stateData.UpdateService(svc)

	s.logger.Info("load balancer created", "service_key", req.ServiceKey)
	return &routerv1.CreateLoadBalancerResponse{}, nil
}

func (s *RouterServer) UpdateLoadBalancer(ctx context.Context, req *routerv1.UpdateLoadBalancerRequest) (*routerv1.UpdateLoadBalancerResponse, error) {
	s.logger.Info("UpdateLoadBalancer request", "service_key", req.ServiceKey, "ports", len(req.Ports))

	if req.ServiceKey == "" {
		s.logger.Error("service_key is required")
		return nil, status.Error(codes.InvalidArgument, "service_key is required")
	}

	svc, ok := s.stateData.GetService(req.ServiceKey)
	if !ok {
		s.logger.Error("service not found", "service_key", req.ServiceKey)
		return nil, status.Error(codes.NotFound, "service not found")
	}

	geos, err := s.geosFromRequest(req.AllowedGeos)
	if err != nil {
		return nil, err
	}

	svc.Targets = protoPortsToState(req.Ports)
	svc.AllowedGeos = geos
	s.stateData.UpdateService(svc)

	s.logger.Info("load balancer updated", "service_key", req.ServiceKey)
	return &routerv1.UpdateLoadBalancerResponse{}, nil
}

func (s *RouterServer) DeleteLoadBalancer(ctx context.Context, req *routerv1.DeleteLoadBalancerRequest) (*routerv1.DeleteLoadBalancerResponse, error) {
	s.logger.Info("DeleteLoadBalancer request", "service_key", req.ServiceKey)

	if req.ServiceKey == "" {
		s.logger.Error("service_key is required")
		return nil, status.Error(codes.InvalidArgument, "service_key is required")
	}

	s.stateData.DeleteService(req.ServiceKey)

	s.logger.Info("load balancer deleted", "service_key", req.ServiceKey)
	return &routerv1.DeleteLoadBalancerResponse{}, nil
}

func (s *RouterServer) GetLoadBalancer(ctx context.Context, req *routerv1.GetLoadBalancerRequest) (*routerv1.GetLoadBalancerResponse, error) {
	s.logger.Debug("GetLoadBalancer request", "service_key", req.ServiceKey)

	if req.ServiceKey == "" {
		s.logger.Error("service_key is required")
		return nil, status.Error(codes.InvalidArgument, "service_key is required")
	}

	svc, ok := s.stateData.GetService(req.ServiceKey)
	if !ok {
		s.logger.Error("service not found", "service_key", req.ServiceKey)
		return nil, status.Error(codes.NotFound, "service not found")
	}

	s.logger.Debug("load balancer found", "service_key", req.ServiceKey)
	resp := &routerv1.GetLoadBalancerResponse{
		ServiceKey:  req.ServiceKey,
		Ports:       statePortsToProto(svc.Targets),
		AllowedGeos: svc.AllowedGeos,
	}
	if svc.SourceAddress != nil {
		resp.AssignedIp = svc.SourceAddress.String()
	}
	if svc.DHCPAcquired != nil {
		resp.DhcpAcquired = *svc.DHCPAcquired
	}
	if svc.Interface != nil {
		resp.InterfaceName = *svc.Interface
	}
	return resp, nil
}

func (s *RouterServer) ListLoadBalancers(ctx context.Context, req *routerv1.ListLoadBalancersRequest) (*routerv1.ListLoadBalancersResponse, error) {
	s.logger.Debug("ListLoadBalancers request")

	// Snapshot state to iterate safely without holding locks.
	snap := s.stateData.Clone()

	var lbs []*routerv1.GetLoadBalancerResponse
	for key, svc := range snap.Services {
		resp := &routerv1.GetLoadBalancerResponse{
			ServiceKey:  key,
			Ports:       statePortsToProto(svc.Targets),
			AllowedGeos: svc.AllowedGeos,
		}
		// Mirror GetLoadBalancer: a DHCP-pending service has no source
		// address yet. net.IP(nil).String() returns "<nil>", which the
		// controller's IP poller would otherwise write into Service
		// status as a literal IP. Leave the field empty instead.
		if svc.SourceAddress != nil {
			resp.AssignedIp = svc.SourceAddress.String()
		}
		if svc.DHCPAcquired != nil {
			resp.DhcpAcquired = *svc.DHCPAcquired
		}
		if svc.Interface != nil {
			resp.InterfaceName = *svc.Interface
		}
		lbs = append(lbs, resp)
	}

	s.logger.Debug("load balancers listed", "count", len(lbs))
	return &routerv1.ListLoadBalancersResponse{LoadBalancers: lbs}, nil
}

func (s *RouterServer) Health(ctx context.Context, req *routerv1.HealthRequest) (*routerv1.HealthResponse, error) {
	s.logger.Debug("Health check request")

	var count int32
	if s.ActiveMacvlanFunc != nil {
		count = s.ActiveMacvlanFunc()
	}
	s.logger.Debug("health check", "healthy", true, "active_macvlans", count)

	return &routerv1.HealthResponse{
		Healthy:        true,
		ActiveMacvlans: count,
	}, nil
}

// geosFromRequest normalises and validates the requested country codes.
// Requests carrying geos are rejected while MaxMind is not configured.
func (s *RouterServer) geosFromRequest(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if !s.GeoEnabled {
		s.logger.Error("allowed_geos supplied but geo-blocking is not configured")
		return nil, status.Error(codes.InvalidArgument, "geo-blocking is not configured; no geo database is available")
	}

	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		cc := strings.ToUpper(strings.TrimSpace(raw))
		if !isoAlpha2RE.MatchString(cc) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid ISO-3166 alpha-2 code %q", raw)
		}
		if _, dup := seen[cc]; dup {
			continue
		}
		seen[cc] = struct{}{}
		out = append(out, cc)
	}
	return out, nil
}

func protoPortsToState(ports []*routerv1.PortMapping) []state.LBTarget {
	result := make([]state.LBTarget, len(ports))
	for i, p := range ports {
		result[i] = state.LBTarget{
			Protocol: p.Protocol,
			SrcPort:  uint16(p.SrcPort),
			Dst:      make([]state.LBDestination, len(p.GetDst())),
		}
		for j, addr := range p.GetDst() {
			result[i].Dst[j] = state.LBDestination{
				Port:    uint16(addr.GetPort()),
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
			SrcPort:  uint32(p.SrcPort),
			Dst:      make([]*routerv1.Destination, len(p.Dst)),
		}
		for j, d := range p.Dst {
			result[i].Dst[j] = &routerv1.Destination{
				Port: uint32(d.Port),
				Addr: d.Address.String(),
			}
		}
	}
	return result
}
