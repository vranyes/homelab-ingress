package router

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	routerv1 "github.com/vranyes/homelab-ingress/controller/pkg/api/router/v1"
)

type fakeServer struct {
	routerv1.UnimplementedRouterServiceServer
	mu          sync.Mutex
	services    map[string]*routerv1.GetLoadBalancerResponse
	createErr   error
	updateErr   error
	deleteErr   error
	getErr      error
	listErr     error
	healthErr   error
	createCalls []string
	updateCalls []string
	deleteCalls []string
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		services: make(map[string]*routerv1.GetLoadBalancerResponse),
	}
}

func (f *fakeServer) CreateLoadBalancer(_ context.Context, req *routerv1.CreateLoadBalancerRequest) (*routerv1.CreateLoadBalancerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, req.ServiceKey)
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.services[req.ServiceKey] = &routerv1.GetLoadBalancerResponse{
		ServiceKey:    req.ServiceKey,
		AssignedIp:    "192.168.1.100",
		Ports:         req.Ports,
		DhcpAcquired:  false,
		InterfaceName: "lb-" + req.ServiceKey,
	}
	return &routerv1.CreateLoadBalancerResponse{AssignedIp: "192.168.1.100"}, nil
}

func (f *fakeServer) UpdateLoadBalancer(_ context.Context, req *routerv1.UpdateLoadBalancerRequest) (*routerv1.UpdateLoadBalancerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls = append(f.updateCalls, req.ServiceKey)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if svc, ok := f.services[req.ServiceKey]; ok {
		svc.Ports = req.Ports
	}
	return &routerv1.UpdateLoadBalancerResponse{}, nil
}

func (f *fakeServer) DeleteLoadBalancer(_ context.Context, req *routerv1.DeleteLoadBalancerRequest) (*routerv1.DeleteLoadBalancerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls = append(f.deleteCalls, req.ServiceKey)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	delete(f.services, req.ServiceKey)
	return &routerv1.DeleteLoadBalancerResponse{}, nil
}

func (f *fakeServer) GetLoadBalancer(_ context.Context, req *routerv1.GetLoadBalancerRequest) (*routerv1.GetLoadBalancerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	svc, ok := f.services[req.ServiceKey]
	if !ok {
		return nil, status.Error(codes.NotFound, "service not found")
	}
	return svc, nil
}

func (f *fakeServer) ListLoadBalancers(_ context.Context, _ *routerv1.ListLoadBalancersRequest) (*routerv1.ListLoadBalancersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var lbs []*routerv1.GetLoadBalancerResponse
	for _, svc := range f.services {
		lbs = append(lbs, svc)
	}
	return &routerv1.ListLoadBalancersResponse{LoadBalancers: lbs}, nil
}

func (f *fakeServer) Health(_ context.Context, _ *routerv1.HealthRequest) (*routerv1.HealthResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return &routerv1.HealthResponse{
		Healthy:        true,
		BaseInterface:  "wan.201",
		ActiveMacvlans: int32(len(f.services)),
	}, nil
}

func startFakeServer(t *testing.T, srv *fakeServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	gs := grpc.NewServer()
	routerv1.RegisterRouterServiceServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func newTestClient(t *testing.T, addr string) *GRPCClient {
	t.Helper()
	c, err := NewClient(addr, "")
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestClient_Create(t *testing.T) {
	fake := newFakeServer()
	addr := startFakeServer(t, fake)
	client := newTestClient(t, addr)

	t.Run("creates load balancer", func(t *testing.T) {
		ports := []PortMapping{
			{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.42.0.10"}},
		}
		entry, err := client.Create(context.Background(), "default/myapp", ports)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entry.AssignedIP != "192.168.1.100" {
			t.Errorf("expected IP 192.168.1.100, got %q", entry.AssignedIP)
		}
		if len(fake.createCalls) != 1 || fake.createCalls[0] != "default/myapp" {
			t.Errorf("expected create call for default/myapp")
		}
	})

	t.Run("returns error on server failure", func(t *testing.T) {
		fake.createErr = fmt.Errorf("router down")
		defer func() { fake.createErr = nil }()

		_, err := client.Create(context.Background(), "default/fail", nil)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestClient_Update(t *testing.T) {
	fake := newFakeServer()
	addr := startFakeServer(t, fake)
	client := newTestClient(t, addr)

	fake.services["default/myapp"] = &routerv1.GetLoadBalancerResponse{
		ServiceKey: "default/myapp",
		AssignedIp: "192.168.1.100",
		Ports: []*routerv1.PortMapping{{
			Protocol: "tcp",
			SrcPort:  80,
			Dst:      []*routerv1.Destination{{Addr: "10.42.0.10", Port: 80}},
		}},
		DhcpAcquired:  true,
		InterfaceName: "lb-default-myapp",
	}

	t.Run("updates ports", func(t *testing.T) {
		ports := []PortMapping{
			{Protocol: "tcp", ExternalPort: 443, PodPort: 8443, PodIPs: []string{"10.42.0.10", "10.42.0.11"}},
		}
		err := client.Update(context.Background(), "default/myapp", ports)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(fake.updateCalls) != 1 || fake.updateCalls[0] != "default/myapp" {
			t.Errorf("expected update call")
		}
	})

	t.Run("returns error on server failure", func(t *testing.T) {
		fake.updateErr = fmt.Errorf("router down")
		defer func() { fake.updateErr = nil }()

		err := client.Update(context.Background(), "default/fail", nil)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestClient_Delete(t *testing.T) {
	fake := newFakeServer()
	addr := startFakeServer(t, fake)
	client := newTestClient(t, addr)

	fake.services["default/myapp"] = &routerv1.GetLoadBalancerResponse{
		ServiceKey:    "default/myapp",
		AssignedIp:    "192.168.1.100",
		Ports:         nil,
		DhcpAcquired:  true,
		InterfaceName: "lb-default-myapp",
	}

	t.Run("deletes load balancer", func(t *testing.T) {
		err := client.Delete(context.Background(), "default/myapp")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(fake.deleteCalls) != 1 {
			t.Errorf("expected delete call")
		}
	})

	t.Run("returns error on server failure", func(t *testing.T) {
		fake.deleteErr = fmt.Errorf("router down")
		defer func() { fake.deleteErr = nil }()

		err := client.Delete(context.Background(), "default/fail")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestClient_Get(t *testing.T) {
	fake := newFakeServer()
	addr := startFakeServer(t, fake)
	client := newTestClient(t, addr)

	fake.services["default/myapp"] = &routerv1.GetLoadBalancerResponse{
		ServiceKey: "default/myapp",
		AssignedIp: "81.23.45.67",
		Ports: []*routerv1.PortMapping{{
			Protocol: "tcp",
			SrcPort:  443,
			Dst: []*routerv1.Destination{
				{Addr: "10.42.0.10", Port: 8443},
			}}},
		DhcpAcquired:  true,
		InterfaceName: "lb-default-myapp",
	}

	t.Run("gets existing service", func(t *testing.T) {
		entry, err := client.Get(context.Background(), "default/myapp")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entry.AssignedIP != "81.23.45.67" {
			t.Errorf("expected IP 81.23.45.67, got %q", entry.AssignedIP)
		}
		if !entry.DHCPAcquired {
			t.Error("expected dhcp_acquired=true")
		}
		if len(entry.Ports) != 1 || entry.Ports[0].PodPort != 8443 {
			t.Errorf("port mismatch: %+v", entry.Ports)
		}
	})

	t.Run("returns NotFound for missing service", func(t *testing.T) {
		_, err := client.Get(context.Background(), "default/nonexistent")
		if err == nil {
			t.Fatal("expected error")
		}
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.NotFound {
			t.Errorf("expected NotFound status, got %v", err)
		}
	})
}

func TestClient_List(t *testing.T) {
	fake := newFakeServer()
	addr := startFakeServer(t, fake)
	client := newTestClient(t, addr)

	fake.services["default/app1"] = &routerv1.GetLoadBalancerResponse{
		ServiceKey:    "default/app1",
		AssignedIp:    "10.0.0.1",
		DhcpAcquired:  true,
		InterfaceName: "lb-default-app1",
	}
	fake.services["default/app2"] = &routerv1.GetLoadBalancerResponse{
		ServiceKey:    "default/app2",
		AssignedIp:    "10.0.0.2",
		DhcpAcquired:  false,
		InterfaceName: "lb-default-app2",
	}

	t.Run("lists all services", func(t *testing.T) {
		entries, err := client.List(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(entries))
		}
	})

	t.Run("returns empty list when no services", func(t *testing.T) {
		fake.mu.Lock()
		fake.services = make(map[string]*routerv1.GetLoadBalancerResponse)
		fake.mu.Unlock()

		entries, err := client.List(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("expected empty list, got %d", len(entries))
		}
	})

	t.Run("returns error on server failure", func(t *testing.T) {
		fake.listErr = fmt.Errorf("router down")
		defer func() { fake.listErr = nil }()

		_, err := client.List(context.Background())
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestClient_Health(t *testing.T) {
	fake := newFakeServer()
	addr := startFakeServer(t, fake)
	client := newTestClient(t, addr)

	t.Run("returns healthy status", func(t *testing.T) {
		h, err := client.Health(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !h.Healthy {
			t.Error("expected healthy")
		}
		if h.BaseInterface != "wan.201" {
			t.Errorf("expected wan.201, got %q", h.BaseInterface)
		}
	})

	t.Run("returns error on server failure", func(t *testing.T) {
		fake.healthErr = fmt.Errorf("router down")
		defer func() { fake.healthErr = nil }()

		_, err := client.Health(context.Background())
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestClient_Close(t *testing.T) {
	fake := newFakeServer()
	addr := startFakeServer(t, fake)
	client, err := NewClient(addr, "")
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	err = client.Close()
	if err != nil {
		t.Fatalf("unexpected error on close: %v", err)
	}
}

func TestToProtoPorts(t *testing.T) {
	ports := []PortMapping{
		{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1", "10.0.0.2"}},
		{Protocol: "udp", ExternalPort: 53, PodPort: 5353, PodIPs: []string{"10.0.0.3"}},
	}
	result := toProtoPorts(ports)
	if len(result) != 2 {
		t.Fatalf("expected 2 proto ports, got %d", len(result))
	}
	if result[0].Protocol != "tcp" {
		t.Errorf("expected tcp, got %q", result[0].Protocol)
	}
	if len(result[0].Dst) != 2 {
		t.Errorf("expected 2 pod IPs, got %d", len(result[0].Dst))
	}
}

func TestFromProto(t *testing.T) {
	resp := &routerv1.GetLoadBalancerResponse{
		ServiceKey:    "ns/svc",
		AssignedIp:    "1.2.3.4",
		DhcpAcquired:  true,
		InterfaceName: "lb-ns-svc",
		Ports: []*routerv1.PortMapping{
			{Protocol: "tcp", SrcPort: 443, Dst: []*routerv1.Destination{
				{Addr: "10.0.0.1", Port: 8443},
			}},
		},
	}
	entry := fromProto(resp)
	if entry.ServiceKey != "ns/svc" {
		t.Errorf("expected ns/svc, got %q", entry.ServiceKey)
	}
	if entry.AssignedIP != "1.2.3.4" {
		t.Errorf("expected 1.2.3.4, got %q", entry.AssignedIP)
	}
	if !entry.DHCPAcquired {
		t.Error("expected dhcp_acquired=true")
	}
	if entry.Interface != "lb-ns-svc" {
		t.Errorf("expected lb-ns-svc, got %q", entry.Interface)
	}
	if len(entry.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(entry.Ports))
	}
}

func TestTokenFileCredentials(t *testing.T) {
	t.Run("reads token from file and sets authorization header", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "token")
		if err := os.WriteFile(path, []byte("  my-secret-token\n"), 0600); err != nil {
			t.Fatal(err)
		}

		creds := &tokenFileCredentials{tokenPath: path}
		md, err := creds.GetRequestMetadata(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := md["authorization"]; got != "Bearer my-secret-token" {
			t.Errorf("expected 'Bearer my-secret-token', got %q", got)
		}
	})

	t.Run("returns error when token file is missing", func(t *testing.T) {
		creds := &tokenFileCredentials{tokenPath: "/nonexistent/token"}
		_, err := creds.GetRequestMetadata(context.Background())
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("does not require transport security", func(t *testing.T) {
		creds := &tokenFileCredentials{tokenPath: "/any"}
		if creds.RequireTransportSecurity() {
			t.Error("expected RequireTransportSecurity to return false")
		}
	})
}
