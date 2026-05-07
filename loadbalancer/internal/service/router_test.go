package service

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/vranyes/homelab-ingress/loadbalancer/internal/dhcp"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/firewall"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/macvlan"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
	routerv1 "github.com/vranyes/homelab-ingress/loadbalancer/pkg/api/router/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type mockLinkManager struct {
	mu          sync.Mutex
	created     map[string]mockIface
	deleteCalls []string
	setUpCalls  []string
	existsMap   map[string]bool
	createErr   error
	deleteErr   error
	setUpErr    error
}

type mockIface struct {
	name   string
	parent string
	mac    net.HardwareAddr
}

func newMockLinkManager() *mockLinkManager {
	return &mockLinkManager{
		created:   make(map[string]mockIface),
		existsMap: make(map[string]bool),
	}
}

func (m *mockLinkManager) Create(iface, parent string, mac net.HardwareAddr) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return m.createErr
	}
	m.created[iface] = mockIface{name: iface, parent: parent, mac: mac}
	return nil
}

func (m *mockLinkManager) Delete(iface string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls = append(m.deleteCalls, iface)
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.created, iface)
	return nil
}

func (m *mockLinkManager) Exists(iface string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.existsMap[iface]; ok {
		return v
	}
	_, ok := m.created[iface]
	return ok
}

func (m *mockLinkManager) GetMAC(iface string) (net.HardwareAddr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if iface, ok := m.created[iface]; ok {
		return iface.mac, nil
	}
	return nil, macvlan.ErrInterfaceNotFound
}

func (m *mockLinkManager) SetUp(iface string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setUpCalls = append(m.setUpCalls, iface)
	if m.setUpErr != nil {
		return m.setUpErr
	}
	return nil
}

func (m *mockLinkManager) setCreateErr(err error) {
	m.createErr = err
}

func (m *mockLinkManager) setDeleteErr(err error) {
	m.deleteErr = err
}

func (m *mockLinkManager) setSetUpErr(err error) {
	m.setUpErr = err
}

func (m *mockLinkManager) setExists(iface string, exists bool) {
	m.existsMap[iface] = exists
}

type mockDHCPClient struct {
	mu        sync.Mutex
	started   bool
	stopped   bool
	startErr  error
	currentIP net.IP
	acquired  bool
	iface     string
	mac       net.HardwareAddr
	onChange  dhcp.IPChangeFunc
}

func (m *mockDHCPClient) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = true
	return m.startErr
}

func (m *mockDHCPClient) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
}

func (m *mockDHCPClient) CurrentIP() net.IP {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentIP
}

func (m *mockDHCPClient) IsAcquired() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acquired
}

func (m *mockDHCPClient) setIP(ip net.IP) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentIP = ip
	m.acquired = ip != nil
}

func newMockDHCPFactory() func(iface string, mac net.HardwareAddr, onChange dhcp.IPChangeFunc) dhcp.Client {
	return func(iface string, mac net.HardwareAddr, onChange dhcp.IPChangeFunc) dhcp.Client {
		return &mockDHCPClient{iface: iface, mac: mac, onChange: onChange}
	}
}

func getMockClient(client dhcp.Client) *mockDHCPClient {
	if mc, ok := client.(*mockDHCPClient); ok {
		return mc
	}
	return nil
}

type addServiceRulesCall struct {
	iface      string
	extIP      string
	ports      []firewall.PortRule
	serviceKey string
}

type mockFirewallManager struct {
	mu              sync.Mutex
	addServiceCalls []addServiceRulesCall
	removeKeys      []string
	addServiceErr   error
}

func (m *mockFirewallManager) AddServiceRules(iface, extIP string, ports []firewall.PortRule, serviceKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addServiceCalls = append(m.addServiceCalls, addServiceRulesCall{
		iface:      iface,
		extIP:      extIP,
		ports:      append([]firewall.PortRule(nil), ports...),
		serviceKey: serviceKey,
	})
	return m.addServiceErr
}

func (m *mockFirewallManager) RemoveRules(serviceKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeKeys = append(m.removeKeys, serviceKey)
	return nil
}

func (m *mockFirewallManager) getAddServiceCalls() []addServiceRulesCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]addServiceRulesCall(nil), m.addServiceCalls...)
}

func (m *mockFirewallManager) resetCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addServiceCalls = nil
	m.removeKeys = nil
}

func newTestService(t *testing.T) (*RouterServer, *mockLinkManager, *dhcp.Manager, *mockFirewallManager, *state.Store, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := dir + "/state.json"

	linkMgr := newMockLinkManager()
	dhcpMgr := dhcp.NewManagerWithFactory(nil, newMockDHCPFactory())

	fwMgr := &mockFirewallManager{}

	st := state.NewState("wan.201")
	store := state.NewStore(statePath)

	srv := NewRouterServer("wan.201", linkMgr, dhcpMgr, fwMgr, store, st)
	return srv, linkMgr, dhcpMgr, fwMgr, store, statePath
}

func TestRouterServer_CreateLoadBalancer(t *testing.T) {
	t.Run("creates load balancer successfully", func(t *testing.T) {
		srv, linkMgr, _, _, _, _ := newTestService(t)

		resp, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports: []*routerv1.PortMapping{
				{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIps: []string{"10.42.0.10"}},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp == nil {
			t.Fatal("expected response")
		}

		iface := macvlan.InterfaceName("default/myapp")
		if !linkMgr.Exists(iface) {
			t.Errorf("expected interface %s to be created", iface)
		}
	})

	t.Run("creates macvlan with generated MAC", func(t *testing.T) {
		srv, linkMgr, _, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		iface := macvlan.InterfaceName("default/myapp")
		mac, err := linkMgr.GetMAC(iface)
		if err != nil {
			t.Fatalf("expected to get MAC: %v", err)
		}
		if mac == nil || mac.String() == "" {
			t.Fatal("expected non-empty MAC")
		}
		if mac[0]&0x02 == 0 {
			t.Errorf("expected local bit set in MAC, got %s", mac.String())
		}
	})

	t.Run("brings interface up after creation", func(t *testing.T) {
		srv, linkMgr, _, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		iface := macvlan.InterfaceName("default/myapp")
		found := false
		for _, call := range linkMgr.setUpCalls {
			if call == iface {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected SetUp to be called for %s", iface)
		}
	})

	t.Run("starts DHCP client", func(t *testing.T) {
		srv, _, dhcpMgr, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		iface := macvlan.InterfaceName("default/myapp")
		client, ok := dhcpMgr.Get(iface)
		if !ok {
			t.Fatal("expected DHCP client to be created")
		}
		mc := getMockClient(client)
		if mc == nil {
			t.Fatal("expected mock client")
		}
		if !mc.started {
			t.Fatal("expected DHCP client to be started")
		}
	})

	t.Run("persists state to disk", func(t *testing.T) {
		srv, _, _, _, store, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		loaded, err := store.Load()
		if err != nil {
			t.Fatalf("failed to load state: %v", err)
		}
		if _, ok := loaded.Services["default/myapp"]; !ok {
			t.Fatal("expected service to be persisted")
		}
	})

	t.Run("returns error when service key is empty", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err == nil {
			t.Fatal("expected error for empty service key")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.InvalidArgument {
			t.Errorf("expected InvalidArgument, got %v", st.Code())
		}
	})

	t.Run("returns error when no ports provided", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{},
		})
		if err == nil {
			t.Fatal("expected error for empty ports")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.InvalidArgument {
			t.Errorf("expected InvalidArgument, got %v", st.Code())
		}
	})

	t.Run("returns AlreadyExists when service already exists", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("first create failed: %v", err)
		}

		_, err = srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err == nil {
			t.Fatal("expected error for duplicate service")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.AlreadyExists {
			t.Errorf("expected AlreadyExists, got %v", st.Code())
		}
	})

	t.Run("returns error when macvlan creation fails", func(t *testing.T) {
		srv, linkMgr, _, _, _, _ := newTestService(t)
		linkMgr.setCreateErr(net.ErrClosed)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err == nil {
			t.Fatal("expected error when macvlan creation fails")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.Internal {
			t.Errorf("expected Internal, got %v", st.Code())
		}
	})

	t.Run("returns error when SetUp fails", func(t *testing.T) {
		srv, linkMgr, _, _, _, _ := newTestService(t)
		linkMgr.setSetUpErr(net.ErrClosed)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err == nil {
			t.Fatal("expected error when SetUp fails")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.Internal {
			t.Errorf("expected Internal, got %v", st.Code())
		}
	})

	t.Run("creates firewall rules with pod IPs", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports: []*routerv1.PortMapping{
				{Protocol: "tcp", ExternalPort: 443, PodPort: 8443, PodIps: []string{"10.42.0.10", "10.42.0.11"}},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestRouterServer_UpdateLoadBalancer(t *testing.T) {
	t.Run("updates ports for existing service", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}

		_, err = srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports: []*routerv1.PortMapping{
				{Protocol: "tcp", ExternalPort: 443, PodPort: 8443, PodIps: []string{"10.42.0.10"}},
			},
		})
		if err != nil {
			t.Fatalf("update failed: %v", err)
		}

		svc, ok := state.GetService(srv.stateData, "default/myapp")
		if !ok {
			t.Fatal("expected service to exist")
		}
		if len(svc.Ports) != 1 || svc.Ports[0].ExternalPort != 443 {
			t.Errorf("expected port 443, got %+v", svc.Ports)
		}
	})

	t.Run("multi-port update applies all ports atomically", func(t *testing.T) {
		srv, _, _, mockFW, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIps: []string{"10.42.0.1"}}},
		})
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}

		// Simulate the service having an assigned external IP so firewall rules
		// will be applied when updating ports.
		state.UpdateIP(srv.stateData, "default/myapp", "81.23.45.67")

		mockFW.resetCalls()

		_, err = srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports: []*routerv1.PortMapping{
				{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIps: []string{"10.42.0.1"}},
				{Protocol: "tcp", ExternalPort: 443, PodPort: 8443, PodIps: []string{"10.42.0.1"}},
				{Protocol: "udp", ExternalPort: 53, PodPort: 5353, PodIps: []string{"10.42.0.1"}},
			},
		})
		if err != nil {
			t.Fatalf("update failed: %v", err)
		}

		calls := mockFW.getAddServiceCalls()
		if len(calls) != 1 {
			t.Fatalf("expected 1 AddServiceRules call, got %d (multi-port update must not recreate chain between ports)", len(calls))
		}
		if len(calls[0].ports) != 3 {
			t.Errorf("expected 3 ports in AddServiceRules call, got %d", len(calls[0].ports))
		}
	})

	t.Run("returns NotFound for missing service", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
			ServiceKey: "default/nonexistent",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err == nil {
			t.Fatal("expected error for missing service")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.NotFound {
			t.Errorf("expected NotFound, got %v", st.Code())
		}
	})

	t.Run("returns error when service key is empty", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
			ServiceKey: "",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err == nil {
			t.Fatal("expected error for empty service key")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.InvalidArgument {
			t.Errorf("expected InvalidArgument, got %v", st.Code())
		}
	})

	t.Run("persists updated state", func(t *testing.T) {
		srv, _, _, _, store, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}

		_, err = srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports: []*routerv1.PortMapping{
				{Protocol: "tcp", ExternalPort: 443, PodPort: 8443, PodIps: []string{"10.42.0.10", "10.42.0.11"}},
			},
		})
		if err != nil {
			t.Fatalf("update failed: %v", err)
		}

		loaded, err := store.Load()
		if err != nil {
			t.Fatalf("failed to load state: %v", err)
		}
		svc := loaded.Services["default/myapp"]
		if len(svc.Ports) != 1 || len(svc.Ports[0].PodIPs) != 2 {
			t.Errorf("expected updated ports with 2 pod IPs, got %+v", svc.Ports)
		}
	})
}

func TestRouterServer_DeleteLoadBalancer(t *testing.T) {
	t.Run("deletes existing service", func(t *testing.T) {
		srv, linkMgr, dhcpMgr, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}

		_, err = srv.DeleteLoadBalancer(context.Background(), &routerv1.DeleteLoadBalancerRequest{
			ServiceKey: "default/myapp",
		})
		if err != nil {
			t.Fatalf("delete failed: %v", err)
		}

		if _, ok := state.GetService(srv.stateData, "default/myapp"); ok {
			t.Fatal("expected service to be deleted from state")
		}

		iface := macvlan.InterfaceName("default/myapp")
		found := false
		for _, call := range linkMgr.deleteCalls {
			if call == iface {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected interface %s to be deleted", iface)
		}

		client, ok := dhcpMgr.Get(iface)
		if ok {
			mc := getMockClient(client)
			if mc != nil && !mc.stopped {
				t.Fatal("expected DHCP client to be stopped")
			}
		}
	})

	t.Run("returns success when service does not exist (idempotent)", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.DeleteLoadBalancer(context.Background(), &routerv1.DeleteLoadBalancerRequest{
			ServiceKey: "default/nonexistent",
		})
		if err != nil {
			t.Fatalf("expected no error for idempotent delete, got %v", err)
		}
	})

	t.Run("returns error when service key is empty", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.DeleteLoadBalancer(context.Background(), &routerv1.DeleteLoadBalancerRequest{
			ServiceKey: "",
		})
		if err == nil {
			t.Fatal("expected error for empty service key")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.InvalidArgument {
			t.Errorf("expected InvalidArgument, got %v", st.Code())
		}
	})

	t.Run("returns error when interface deletion fails", func(t *testing.T) {
		srv, linkMgr, _, _, _, _ := newTestService(t)
		linkMgr.setDeleteErr(net.ErrClosed)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}

		_, err = srv.DeleteLoadBalancer(context.Background(), &routerv1.DeleteLoadBalancerRequest{
			ServiceKey: "default/myapp",
		})
		if err == nil {
			t.Fatal("expected error when interface deletion fails")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.Internal {
			t.Errorf("expected Internal, got %v", st.Code())
		}
	})

	t.Run("persists deleted state", func(t *testing.T) {
		srv, _, _, _, store, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}

		_, err = srv.DeleteLoadBalancer(context.Background(), &routerv1.DeleteLoadBalancerRequest{
			ServiceKey: "default/myapp",
		})
		if err != nil {
			t.Fatalf("delete failed: %v", err)
		}

		loaded, err := store.Load()
		if err != nil {
			t.Fatalf("failed to load state: %v", err)
		}
		if _, ok := loaded.Services["default/myapp"]; ok {
			t.Fatal("expected service to be removed from persisted state")
		}
	})
}

func TestRouterServer_GetLoadBalancer(t *testing.T) {
	t.Run("returns service details", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports: []*routerv1.PortMapping{
				{Protocol: "tcp", ExternalPort: 443, PodPort: 8443, PodIps: []string{"10.42.0.10"}},
			},
		})
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}

		resp, err := srv.GetLoadBalancer(context.Background(), &routerv1.GetLoadBalancerRequest{
			ServiceKey: "default/myapp",
		})
		if err != nil {
			t.Fatalf("get failed: %v", err)
		}
		if resp.ServiceKey != "default/myapp" {
			t.Errorf("expected service_key default/myapp, got %q", resp.ServiceKey)
		}
		if resp.InterfaceName != macvlan.InterfaceName("default/myapp") {
			t.Errorf("expected interface %s, got %q", macvlan.InterfaceName("default/myapp"), resp.InterfaceName)
		}
		if len(resp.Ports) != 1 {
			t.Fatalf("expected 1 port, got %d", len(resp.Ports))
		}
		if resp.Ports[0].ExternalPort != 443 {
			t.Errorf("expected external port 443, got %d", resp.Ports[0].ExternalPort)
		}
		if resp.Ports[0].PodPort != 8443 {
			t.Errorf("expected pod port 8443, got %d", resp.Ports[0].PodPort)
		}
	})

	t.Run("returns NotFound for missing service", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.GetLoadBalancer(context.Background(), &routerv1.GetLoadBalancerRequest{
			ServiceKey: "default/nonexistent",
		})
		if err == nil {
			t.Fatal("expected error for missing service")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.NotFound {
			t.Errorf("expected NotFound, got %v", st.Code())
		}
	})

	t.Run("returns error when service key is empty", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.GetLoadBalancer(context.Background(), &routerv1.GetLoadBalancerRequest{
			ServiceKey: "",
		})
		if err == nil {
			t.Fatal("expected error for empty service key")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatal("expected gRPC status error")
		}
		if st.Code() != codes.InvalidArgument {
			t.Errorf("expected InvalidArgument, got %v", st.Code())
		}
	})

	t.Run("reflects DHCP acquired status", func(t *testing.T) {
		srv, _, dhcpMgr, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/myapp",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}

		iface := macvlan.InterfaceName("default/myapp")
		client, _ := dhcpMgr.Get(iface)
		mc := getMockClient(client)
		if mc != nil {
			mc.setIP(net.ParseIP("81.23.45.67"))
			state.UpdateIP(srv.stateData, "default/myapp", "81.23.45.67")
		}

		resp, err := srv.GetLoadBalancer(context.Background(), &routerv1.GetLoadBalancerRequest{
			ServiceKey: "default/myapp",
		})
		if err != nil {
			t.Fatalf("get failed: %v", err)
		}
		if resp.AssignedIp != "81.23.45.67" {
			t.Errorf("expected IP 81.23.45.67, got %q", resp.AssignedIp)
		}
		if !resp.DhcpAcquired {
			t.Error("expected dhcp_acquired to be true")
		}
	})
}

func TestRouterServer_ListLoadBalancers(t *testing.T) {
	t.Run("returns empty list when no services", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		resp, err := srv.ListLoadBalancers(context.Background(), &routerv1.ListLoadBalancersRequest{})
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		if len(resp.LoadBalancers) != 0 {
			t.Errorf("expected empty list, got %d items", len(resp.LoadBalancers))
		}
	})

	t.Run("returns all services", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/app1",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("create app1 failed: %v", err)
		}

		_, err = srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/app2",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 443, PodPort: 443, PodIps: []string{"10.42.0.11"}}},
		})
		if err != nil {
			t.Fatalf("create app2 failed: %v", err)
		}

		resp, err := srv.ListLoadBalancers(context.Background(), &routerv1.ListLoadBalancersRequest{})
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		if len(resp.LoadBalancers) != 2 {
			t.Fatalf("expected 2 services, got %d", len(resp.LoadBalancers))
		}
	})
}

func TestRouterServer_Health(t *testing.T) {
	t.Run("returns healthy status", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		resp, err := srv.Health(context.Background(), &routerv1.HealthRequest{})
		if err != nil {
			t.Fatalf("health failed: %v", err)
		}
		if !resp.Healthy {
			t.Error("expected healthy=true")
		}
		if resp.BaseInterface != "wan.201" {
			t.Errorf("expected base_interface wan.201, got %q", resp.BaseInterface)
		}
		if resp.ActiveMacvlans != 0 {
			t.Errorf("expected 0 active macvlans, got %d", resp.ActiveMacvlans)
		}
	})

	t.Run("reports active macvlan count", func(t *testing.T) {
		srv, _, _, _, _, _ := newTestService(t)

		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "default/app1",
			Ports:      []*routerv1.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.42.0.10"}}},
		})
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}

		resp, err := srv.Health(context.Background(), &routerv1.HealthRequest{})
		if err != nil {
			t.Fatalf("health failed: %v", err)
		}
		if resp.ActiveMacvlans != 1 {
			t.Errorf("expected 1 active macvlan, got %d", resp.ActiveMacvlans)
		}
	})
}

func TestProtoPortsToState(t *testing.T) {
	t.Run("converts single port", func(t *testing.T) {
		proto := []*routerv1.PortMapping{
			{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIps: []string{"10.0.0.1"}},
		}
		result := protoPortsToState(proto)
		if len(result) != 1 {
			t.Fatalf("expected 1 port, got %d", len(result))
		}
		if result[0].Protocol != "tcp" {
			t.Errorf("expected tcp, got %q", result[0].Protocol)
		}
		if result[0].ExternalPort != 80 {
			t.Errorf("expected 80, got %d", result[0].ExternalPort)
		}
		if result[0].PodPort != 8080 {
			t.Errorf("expected 8080, got %d", result[0].PodPort)
		}
		if len(result[0].PodIPs) != 1 || result[0].PodIPs[0] != "10.0.0.1" {
			t.Errorf("expected pod IP 10.0.0.1, got %v", result[0].PodIPs)
		}
	})

	t.Run("converts multiple ports", func(t *testing.T) {
		proto := []*routerv1.PortMapping{
			{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIps: []string{"10.0.0.1"}},
			{Protocol: "udp", ExternalPort: 53, PodPort: 5353, PodIps: []string{"10.0.0.2"}},
		}
		result := protoPortsToState(proto)
		if len(result) != 2 {
			t.Fatalf("expected 2 ports, got %d", len(result))
		}
	})

	t.Run("handles empty list", func(t *testing.T) {
		result := protoPortsToState(nil)
		if len(result) != 0 {
			t.Errorf("expected empty list, got %d", len(result))
		}
	})
}

func TestStatePortsToProto(t *testing.T) {
	t.Run("converts single port", func(t *testing.T) {
		statePorts := []state.PortEntry{
			{Protocol: "tcp", ExternalPort: 443, PodPort: 8443, PodIPs: []string{"10.0.0.1"}},
		}
		result := statePortsToProto(statePorts)
		if len(result) != 1 {
			t.Fatalf("expected 1 port, got %d", len(result))
		}
		if result[0].Protocol != "tcp" {
			t.Errorf("expected tcp, got %q", result[0].Protocol)
		}
		if result[0].ExternalPort != 443 {
			t.Errorf("expected 443, got %d", result[0].ExternalPort)
		}
		if result[0].PodPort != 8443 {
			t.Errorf("expected 8443, got %d", result[0].PodPort)
		}
	})

	t.Run("handles empty list", func(t *testing.T) {
		result := statePortsToProto(nil)
		if len(result) != 0 {
			t.Errorf("expected empty list, got %d", len(result))
		}
	})
}
