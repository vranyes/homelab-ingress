package router

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	routerv1 "github.com/vranyes/homelab-ingress/controller/pkg/api/router/v1"
)

type fakeServer struct {
	routerv1.UnimplementedRouterServiceServer
	mu           sync.Mutex
	services     map[string]*routerv1.GetLoadBalancerResponse
	createErr    error
	updateErr    error
	deleteErr    error
	getErr       error
	listErr      error
	healthErr    error
	createCalls  []string
	updateCalls  []string
	deleteCalls  []string
	createGeos   map[string][]string
	updateGeos   map[string][]string
	createCidrs  map[string][]string
	updateCidrs  map[string][]string
	createDenied map[string][]string
	updateDenied map[string][]string
	createPolicy map[string]string
	updatePolicy map[string]string
	createRate   map[string]*routerv1.RateLimit
	updateRate   map[string]*routerv1.RateLimit
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		services:     make(map[string]*routerv1.GetLoadBalancerResponse),
		createGeos:   make(map[string][]string),
		updateGeos:   make(map[string][]string),
		createCidrs:  make(map[string][]string),
		updateCidrs:  make(map[string][]string),
		createDenied: make(map[string][]string),
		updateDenied: make(map[string][]string),
		createPolicy: make(map[string]string),
		updatePolicy: make(map[string]string),
		createRate:   make(map[string]*routerv1.RateLimit),
		updateRate:   make(map[string]*routerv1.RateLimit),
	}
}

func (f *fakeServer) CreateLoadBalancer(_ context.Context, req *routerv1.CreateLoadBalancerRequest) (*routerv1.CreateLoadBalancerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, req.ServiceKey)
	f.createGeos[req.ServiceKey] = append([]string(nil), req.AllowedGeos...)
	f.createCidrs[req.ServiceKey] = append([]string(nil), req.AllowedCidrs...)
	f.createDenied[req.ServiceKey] = append([]string(nil), req.DeniedCidrs...)
	f.createPolicy[req.ServiceKey] = req.LbPolicy
	f.createRate[req.ServiceKey] = req.RateLimit
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.services[req.ServiceKey] = &routerv1.GetLoadBalancerResponse{
		ServiceKey:    req.ServiceKey,
		AssignedIp:    "192.168.1.100",
		Ports:         req.Ports,
		AllowedGeos:   req.AllowedGeos,
		AllowedCidrs:  req.AllowedCidrs,
		DeniedCidrs:   req.DeniedCidrs,
		LbPolicy:      req.LbPolicy,
		RateLimit:     req.RateLimit,
		DhcpAcquired:  false,
		InterfaceName: "lb-" + req.ServiceKey,
	}
	return &routerv1.CreateLoadBalancerResponse{AssignedIp: "192.168.1.100"}, nil
}

func (f *fakeServer) UpdateLoadBalancer(_ context.Context, req *routerv1.UpdateLoadBalancerRequest) (*routerv1.UpdateLoadBalancerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls = append(f.updateCalls, req.ServiceKey)
	f.updateGeos[req.ServiceKey] = append([]string(nil), req.AllowedGeos...)
	f.updateCidrs[req.ServiceKey] = append([]string(nil), req.AllowedCidrs...)
	f.updateDenied[req.ServiceKey] = append([]string(nil), req.DeniedCidrs...)
	f.updatePolicy[req.ServiceKey] = req.LbPolicy
	f.updateRate[req.ServiceKey] = req.RateLimit
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if svc, ok := f.services[req.ServiceKey]; ok {
		svc.Ports = req.Ports
		svc.AllowedGeos = req.AllowedGeos
		svc.AllowedCidrs = req.AllowedCidrs
		svc.DeniedCidrs = req.DeniedCidrs
		svc.LbPolicy = req.LbPolicy
		svc.RateLimit = req.RateLimit
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

func generateTestCert(t *testing.T) (*tls.Config, []byte) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	caTmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	serial, _ = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	leafTmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{leafDER, caDER},
			PrivateKey:  leafKey,
		}},
		MinVersion: tls.VersionTLS12,
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return serverTLS, caPEM
}

func startFakeServerTLS(t *testing.T, srv *fakeServer) (addr string, caPEM []byte) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	tlsCfg, caPEM := generateTestCert(t)
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	routerv1.RegisterRouterServiceServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	return lis.Addr().String(), caPEM
}

func newTestClient(t *testing.T, addr string, caPEM []byte) *GRPCClient {
	t.Helper()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0644); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0644); err != nil {
		t.Fatalf("write token: %v", err)
	}
	c, err := NewClient(addr, tokenPath, caPath)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func newTestClientWithToken(t *testing.T, addr string, caPEM []byte, token string) *GRPCClient {
	t.Helper()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0644); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0644); err != nil {
		t.Fatalf("write token: %v", err)
	}
	c, err := NewClient(addr, tokenPath, caPath)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestClient_Create(t *testing.T) {
	fake := newFakeServer()
	addr, caPEM := startFakeServerTLS(t, fake)
	client := newTestClient(t, addr, caPEM)

	t.Run("creates load balancer", func(t *testing.T) {
		ports := []PortMapping{
			{Protocol: "tcp", ExternalPort: 80, Destinations: []Destination{{Addr: "10.42.0.10", Port: 8080}}},
		}
		entry, err := client.Create(context.Background(), "default/myapp", ports, []string{"US", "CA"}, []string{"192.168.1.0/24"}, []string{"203.0.113.0/24"}, "hash-source-ip", &RateLimit{Rate: 5, Burst: 10})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entry.RateLimit == nil || entry.RateLimit.Rate != 5 || entry.RateLimit.Burst != 10 {
			t.Errorf("entry rate limit = %+v", entry.RateLimit)
		}
		if got := fake.createRate["default/myapp"]; got == nil || got.Rate != 5 || got.Burst != 10 {
			t.Errorf("request rate limit = %+v", got)
		}
		if entry.AssignedIP != "192.168.1.100" {
			t.Errorf("expected IP 192.168.1.100, got %q", entry.AssignedIP)
		}
		if len(entry.AllowedGeos) != 2 || entry.AllowedGeos[0] != "US" || entry.AllowedGeos[1] != "CA" {
			t.Errorf("entry geos = %v", entry.AllowedGeos)
		}
		if got := fake.createGeos["default/myapp"]; len(got) != 2 || got[0] != "US" || got[1] != "CA" {
			t.Errorf("request geos = %v", got)
		}
		if got := fake.createCidrs["default/myapp"]; len(got) != 1 || got[0] != "192.168.1.0/24" {
			t.Errorf("request cidrs = %v", got)
		}
		if got := fake.createDenied["default/myapp"]; len(got) != 1 || got[0] != "203.0.113.0/24" {
			t.Errorf("request denied cidrs = %v", got)
		}
		if got := fake.createPolicy["default/myapp"]; got != "hash-source-ip" {
			t.Errorf("request lb policy = %q", got)
		}
		if len(entry.AllowedCIDRs) != 1 || entry.AllowedCIDRs[0] != "192.168.1.0/24" {
			t.Errorf("entry cidrs = %v", entry.AllowedCIDRs)
		}
		if len(entry.DeniedCIDRs) != 1 || entry.DeniedCIDRs[0] != "203.0.113.0/24" {
			t.Errorf("entry denied cidrs = %v", entry.DeniedCIDRs)
		}
		if entry.LBPolicy != "hash-source-ip" {
			t.Errorf("entry lb policy = %q", entry.LBPolicy)
		}
		if len(fake.createCalls) != 1 || fake.createCalls[0] != "default/myapp" {
			t.Errorf("expected create call for default/myapp")
		}
	})

	t.Run("returns error on server failure", func(t *testing.T) {
		fake.createErr = fmt.Errorf("router down")
		defer func() { fake.createErr = nil }()

		_, err := client.Create(context.Background(), "default/fail", nil, nil, nil, nil, "", nil)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestClient_Update(t *testing.T) {
	fake := newFakeServer()
	addr, caPEM := startFakeServerTLS(t, fake)
	client := newTestClient(t, addr, caPEM)

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
			{Protocol: "tcp", ExternalPort: 443, Destinations: []Destination{{Addr: "10.42.0.10", Port: 8443}, {Addr: "10.42.0.11", Port: 8443}}},
		}
		err := client.Update(context.Background(), "default/myapp", ports, []string{"EU"}, []string{"10.0.0.0/8"}, []string{"203.0.113.0/24"}, "random", &RateLimit{Rate: 7, Burst: 9})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := fake.updateGeos["default/myapp"]; len(got) != 1 || got[0] != "EU" {
			t.Errorf("update geos = %v", got)
		}
		if got := fake.updateCidrs["default/myapp"]; len(got) != 1 || got[0] != "10.0.0.0/8" {
			t.Errorf("update cidrs = %v", got)
		}
		if got := fake.updateDenied["default/myapp"]; len(got) != 1 || got[0] != "203.0.113.0/24" {
			t.Errorf("update denied cidrs = %v", got)
		}
		if got := fake.updatePolicy["default/myapp"]; got != "random" {
			t.Errorf("update lb policy = %q", got)
		}
		if got := fake.updateRate["default/myapp"]; got == nil || got.Rate != 7 || got.Burst != 9 {
			t.Errorf("update rate limit = %+v", got)
		}
		if len(fake.updateCalls) != 1 || fake.updateCalls[0] != "default/myapp" {
			t.Errorf("expected update call")
		}
	})

	t.Run("returns error on server failure", func(t *testing.T) {
		fake.updateErr = fmt.Errorf("router down")
		defer func() { fake.updateErr = nil }()

		err := client.Update(context.Background(), "default/fail", nil, nil, nil, nil, "", nil)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestClient_Delete(t *testing.T) {
	fake := newFakeServer()
	addr, caPEM := startFakeServerTLS(t, fake)
	client := newTestClient(t, addr, caPEM)

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
	addr, caPEM := startFakeServerTLS(t, fake)
	client := newTestClient(t, addr, caPEM)

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
		AllowedGeos:   []string{"US"},
		AllowedCidrs:  []string{"192.168.1.0/24"},
		DeniedCidrs:   []string{"203.0.113.0/24"},
		LbPolicy:      "random",
		RateLimit:     &routerv1.RateLimit{Rate: 5, Burst: 10},
	}
	t.Run("gets existing service", func(t *testing.T) {
		entry, err := client.Get(context.Background(), "default/myapp")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entry.RateLimit == nil || entry.RateLimit.Rate != 5 || entry.RateLimit.Burst != 10 {
			t.Errorf("rate limit mismatch: %+v", entry.RateLimit)
		}
		if entry.AssignedIP != "81.23.45.67" {
			t.Errorf("expected IP 81.23.45.67, got %q", entry.AssignedIP)
		}
		if !entry.DHCPAcquired {
			t.Error("expected dhcp_acquired=true")
		}
		if len(entry.Ports) != 1 || entry.Ports[0].Destinations[0].Port != 8443 {
			t.Errorf("port mismatch: %+v", entry.Ports)
		}
		if len(entry.AllowedGeos) != 1 || entry.AllowedGeos[0] != "US" {
			t.Errorf("geos mismatch: %+v", entry.AllowedGeos)
		}
		if len(entry.AllowedCIDRs) != 1 || entry.AllowedCIDRs[0] != "192.168.1.0/24" {
			t.Errorf("cidrs mismatch: %+v", entry.AllowedCIDRs)
		}
		if len(entry.DeniedCIDRs) != 1 || entry.DeniedCIDRs[0] != "203.0.113.0/24" {
			t.Errorf("denied cidrs mismatch: %+v", entry.DeniedCIDRs)
		}
		if entry.LBPolicy != "random" {
			t.Errorf("lb policy mismatch: %q", entry.LBPolicy)
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
	addr, caPEM := startFakeServerTLS(t, fake)
	client := newTestClient(t, addr, caPEM)

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
	addr, caPEM := startFakeServerTLS(t, fake)
	client := newTestClient(t, addr, caPEM)

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
	addr, caPEM := startFakeServerTLS(t, fake)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0644); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0644); err != nil {
		t.Fatalf("write token: %v", err)
	}

	client, err := NewClient(addr, tokenPath, caPath)
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
		{Protocol: "tcp", ExternalPort: 80, Destinations: []Destination{{Addr: "10.0.0.1", Port: 8080}, {Addr: "10.0.0.2", Port: 8080}}},
		{Protocol: "udp", ExternalPort: 53, Destinations: []Destination{{Addr: "10.0.0.3", Port: 5353}}},
	}
	result := toProtoPorts(ports)
	if len(result) != 2 {
		t.Fatalf("expected 2 proto ports, got %d", len(result))
	}
	if result[0].Protocol != "tcp" {
		t.Errorf("expected tcp, got %q", result[0].Protocol)
	}
	if len(result[0].Dst) != 2 {
		t.Errorf("expected 2 destinations, got %d", len(result[0].Dst))
	}
}

func TestFromProto(t *testing.T) {
	resp := &routerv1.GetLoadBalancerResponse{
		ServiceKey:    "ns/svc",
		AssignedIp:    "1.2.3.4",
		DhcpAcquired:  true,
		InterfaceName: "lb-ns-svc",
		RateLimit:     &routerv1.RateLimit{Rate: 3, Burst: 4},
		AllowedCidrs:  []string{"10.0.0.0/8"},
		DeniedCidrs:   []string{"203.0.113.0/24"},
		LbPolicy:      "hash-source-ip",
		Ports: []*routerv1.PortMapping{
			{Protocol: "tcp", SrcPort: 443, Dst: []*routerv1.Destination{
				{Addr: "10.0.0.1", Port: 8443},
				{Addr: "10.0.0.2", Port: 8443},
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
	if entry.RateLimit == nil || entry.RateLimit.Rate != 3 || entry.RateLimit.Burst != 4 {
		t.Errorf("expected rate limit 3/4, got %+v", entry.RateLimit)
	}
	if len(entry.AllowedCIDRs) != 1 || entry.AllowedCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("expected cidrs [10.0.0.0/8], got %+v", entry.AllowedCIDRs)
	}
	if len(entry.DeniedCIDRs) != 1 || entry.DeniedCIDRs[0] != "203.0.113.0/24" {
		t.Errorf("expected denied cidrs [203.0.113.0/24], got %+v", entry.DeniedCIDRs)
	}
	if entry.LBPolicy != "hash-source-ip" {
		t.Errorf("expected lb policy hash-source-ip, got %q", entry.LBPolicy)
	}
	if len(entry.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(entry.Ports))
	}
	if len(entry.Ports[0].Destinations) != 2 {
		t.Fatalf("expected 2 destinations, got %d", len(entry.Ports[0].Destinations))
	}
	if entry.Ports[0].Destinations[0].Addr != "10.0.0.1" || entry.Ports[0].Destinations[0].Port != 8443 {
		t.Errorf("expected first destination 10.0.0.1:8443, got %+v", entry.Ports[0].Destinations[0])
	}
	if entry.Ports[0].Destinations[1].Addr != "10.0.0.2" || entry.Ports[0].Destinations[1].Port != 8443 {
		t.Errorf("expected second destination 10.0.0.2:8443, got %+v", entry.Ports[0].Destinations[1])
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

	t.Run("requires transport security", func(t *testing.T) {
		creds := &tokenFileCredentials{tokenPath: "/any"}
		if !creds.RequireTransportSecurity() {
			t.Error("expected RequireTransportSecurity to return true")
		}
	})
}
