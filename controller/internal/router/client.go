package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	routerv1 "github.com/vranyes/homelab-ingress/controller/pkg/api/router/v1"
)

type ServiceEntry struct {
	ServiceKey   string
	AssignedIP   string
	Ports        []PortMapping
	AllowedGeos  []string
	RateLimit    *RateLimit
	DHCPAcquired bool
	Interface    string
}

// RateLimit is the normalised per-source-IP new-connection limit.
type RateLimit struct {
	Rate  uint64
	Burst uint32
}

type Destination struct {
	Addr string
	Port uint16
}

type PortMapping struct {
	Protocol     string
	ExternalPort uint16
	Destinations []Destination
}

type HealthStatus struct {
	Healthy        bool
	BaseInterface  string
	ActiveMacvlans int32
}

type Client interface {
	Create(ctx context.Context, serviceKey string, ports []PortMapping, geos []string, rateLimit *RateLimit) (*ServiceEntry, error)
	Update(ctx context.Context, serviceKey string, ports []PortMapping, geos []string, rateLimit *RateLimit) error
	Delete(ctx context.Context, serviceKey string) error
	Get(ctx context.Context, serviceKey string) (*ServiceEntry, error)
	List(ctx context.Context) ([]ServiceEntry, error)
	Health(ctx context.Context) (*HealthStatus, error)
	Close() error
}

type GRPCClient struct {
	conn   *grpc.ClientConn
	client routerv1.RouterServiceClient
}

func NewClient(addr string, tokenPath string, caPath string) (*GRPCClient, error) {
	pool := x509.NewCertPool()
	caData, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", caPath, err)
	}
	if !pool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("no valid CA cert in %q", caPath)
	}
	tlsCfg := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}
	opts = append(opts, grpc.WithPerRPCCredentials(&tokenFileCredentials{tokenPath: tokenPath}))
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial router service: %w", err)
	}
	return &GRPCClient{
		conn:   conn,
		client: routerv1.NewRouterServiceClient(conn),
	}, nil
}

// tokenFileCredentials implements credentials.PerRPCCredentials by reading a
// Bearer token from a file on each request. This supports Kubernetes projected
// service account tokens, which are rotated in place.
type tokenFileCredentials struct {
	tokenPath string
}

func (t *tokenFileCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	data, err := os.ReadFile(t.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read token file %q: %w", t.tokenPath, err)
	}
	return map[string]string{
		"authorization": "Bearer " + strings.TrimSpace(string(data)),
	}, nil
}

func (t *tokenFileCredentials) RequireTransportSecurity() bool { return true }

var _ credentials.PerRPCCredentials = (*tokenFileCredentials)(nil)

func (c *GRPCClient) Create(ctx context.Context, serviceKey string, ports []PortMapping, geos []string, rateLimit *RateLimit) (*ServiceEntry, error) {
	req := &routerv1.CreateLoadBalancerRequest{
		ServiceKey:  serviceKey,
		Ports:       toProtoPorts(ports),
		AllowedGeos: geos,
		RateLimit:   toProtoRateLimit(rateLimit),
	}
	resp, err := c.client.CreateLoadBalancer(ctx, req)
	if err != nil {
		return nil, err
	}
	return &ServiceEntry{
		ServiceKey:  serviceKey,
		AssignedIP:  resp.AssignedIp,
		Ports:       ports,
		AllowedGeos: geos,
		RateLimit:   rateLimit,
	}, nil
}

func (c *GRPCClient) Update(ctx context.Context, serviceKey string, ports []PortMapping, geos []string, rateLimit *RateLimit) error {
	req := &routerv1.UpdateLoadBalancerRequest{
		ServiceKey:  serviceKey,
		Ports:       toProtoPorts(ports),
		AllowedGeos: geos,
		RateLimit:   toProtoRateLimit(rateLimit),
	}
	_, err := c.client.UpdateLoadBalancer(ctx, req)
	return err
}

func (c *GRPCClient) Delete(ctx context.Context, serviceKey string) error {
	req := &routerv1.DeleteLoadBalancerRequest{
		ServiceKey: serviceKey,
	}
	_, err := c.client.DeleteLoadBalancer(ctx, req)
	return err
}

func (c *GRPCClient) Get(ctx context.Context, serviceKey string) (*ServiceEntry, error) {
	req := &routerv1.GetLoadBalancerRequest{
		ServiceKey: serviceKey,
	}
	resp, err := c.client.GetLoadBalancer(ctx, req)
	if err != nil {
		return nil, err
	}
	return fromProto(resp), nil
}

func (c *GRPCClient) List(ctx context.Context) ([]ServiceEntry, error) {
	resp, err := c.client.ListLoadBalancers(ctx, &routerv1.ListLoadBalancersRequest{})
	if err != nil {
		return nil, err
	}
	entries := make([]ServiceEntry, len(resp.LoadBalancers))
	for i, lb := range resp.LoadBalancers {
		entries[i] = *fromProto(lb)
	}
	return entries, nil
}

func (c *GRPCClient) Health(ctx context.Context) (*HealthStatus, error) {
	resp, err := c.client.Health(ctx, &routerv1.HealthRequest{})
	if err != nil {
		return nil, err
	}
	return &HealthStatus{
		Healthy:        resp.Healthy,
		BaseInterface:  resp.BaseInterface,
		ActiveMacvlans: resp.ActiveMacvlans,
	}, nil
}

func (c *GRPCClient) Close() error {
	return c.conn.Close()
}

func toProtoPorts(ports []PortMapping) []*routerv1.PortMapping {
	result := make([]*routerv1.PortMapping, len(ports))
	for i, p := range ports {
		result[i] = &routerv1.PortMapping{
			Protocol: p.Protocol,
			SrcPort:  uint32(p.ExternalPort),
			Dst:      make([]*routerv1.Destination, len(p.Destinations)),
		}
		for j, d := range p.Destinations {
			result[i].Dst[j] = &routerv1.Destination{
				Addr: d.Addr,
				Port: uint32(d.Port),
			}
		}
	}
	return result
}

func fromProto(resp *routerv1.GetLoadBalancerResponse) *ServiceEntry {
	ports := make([]PortMapping, len(resp.Ports))
	for i, p := range resp.Ports {
		dst := make([]Destination, len(p.Dst))
		for j, d := range p.Dst {
			dst[j] = Destination{
				Addr: d.Addr,
				Port: uint16(d.Port),
			}
		}
		ports[i] = PortMapping{
			Protocol:     p.Protocol,
			ExternalPort: uint16(p.SrcPort),
			Destinations: dst,
		}
	}
	return &ServiceEntry{
		ServiceKey:   resp.ServiceKey,
		AssignedIP:   resp.AssignedIp,
		Ports:        ports,
		AllowedGeos:  resp.AllowedGeos,
		RateLimit:    fromProtoRateLimit(resp.RateLimit),
		DHCPAcquired: resp.DhcpAcquired,
		Interface:    resp.InterfaceName,
	}
}

func toProtoRateLimit(rl *RateLimit) *routerv1.RateLimit {
	if rl == nil {
		return nil
	}
	return &routerv1.RateLimit{Rate: rl.Rate, Burst: rl.Burst}
}

func fromProtoRateLimit(rl *routerv1.RateLimit) *RateLimit {
	if rl == nil {
		return nil
	}
	return &RateLimit{Rate: rl.Rate, Burst: rl.Burst}
}

func IsErrNotFound(err error) bool {
	if err == nil {
		return false
	}
	return status.Code(err) == codes.NotFound
}

// IsRetryable returns true for gRPC errors that are likely transient and
// where retrying the request later may succeed.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}
