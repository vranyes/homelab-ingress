package recovery

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"

	"github.com/vranyes/homelab-ingress/loadbalancer/internal/dhcp"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/firewall"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testLinkMgr struct {
	mu          sync.Mutex
	existsMap   map[string]bool
	macMap      map[string]net.HardwareAddr
	createCalls []createCall
	deleteCalls []string
	setUpCalls  []string
	createErr   map[string]error
	deleteErr   map[string]error
	setUpErr    map[string]error
}

type createCall struct {
	iface  string
	parent string
	mac    net.HardwareAddr
}

func newTestLinkMgr() *testLinkMgr {
	return &testLinkMgr{
		existsMap: make(map[string]bool),
		macMap:    make(map[string]net.HardwareAddr),
		createErr: make(map[string]error),
		deleteErr: make(map[string]error),
		setUpErr:  make(map[string]error),
	}
}

func (m *testLinkMgr) Create(iface, parent string, mac net.HardwareAddr) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls = append(m.createCalls, createCall{iface, parent, mac})
	m.existsMap[iface] = true
	m.macMap[iface] = mac
	return m.createErr[iface]
}

func (m *testLinkMgr) Delete(iface string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls = append(m.deleteCalls, iface)
	delete(m.existsMap, iface)
	delete(m.macMap, iface)
	return m.deleteErr[iface]
}

func (m *testLinkMgr) Exists(iface string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.existsMap[iface]
}

func (m *testLinkMgr) GetMAC(iface string) (net.HardwareAddr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mac, ok := m.macMap[iface]
	if !ok {
		return nil, nil
	}
	return mac, nil
}

func (m *testLinkMgr) SetUp(iface string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setUpCalls = append(m.setUpCalls, iface)
	return m.setUpErr[iface]
}

func (m *testLinkMgr) setCreateErr(iface string, err error) {
	m.createErr[iface] = err
}

func (m *testLinkMgr) setDeleteErr(iface string, err error) {
	m.deleteErr[iface] = err
}

func (m *testLinkMgr) setSetUpErr(iface string, err error) {
	m.setUpErr[iface] = err
}

func (m *testLinkMgr) preExists(iface string, mac net.HardwareAddr) {
	m.existsMap[iface] = true
	m.macMap[iface] = mac
}

type testDHCPMgr struct {
	mu         sync.Mutex
	newCalls   []dhcpCall
	startCalls []string
	getMap     map[string]dhcp.Client
	startErr   map[string]error
}

type dhcpCall struct {
	iface string
	mac   net.HardwareAddr
}

func newTestDHCPMgr() *testDHCPMgr {
	return &testDHCPMgr{
		getMap:   make(map[string]dhcp.Client),
		startErr: make(map[string]error),
	}
}

func (m *testDHCPMgr) NewAndAdd(iface string, mac net.HardwareAddr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.newCalls = append(m.newCalls, dhcpCall{iface, mac})
}

func (m *testDHCPMgr) Get(iface string) (dhcp.Client, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.getMap[iface]
	return c, ok
}

func (m *testDHCPMgr) Remove(iface string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.getMap, iface)
}

func (m *testDHCPMgr) Start(iface string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startCalls = append(m.startCalls, iface)
	return m.startErr[iface]
}

func (m *testDHCPMgr) setStartErr(iface string, err error) {
	m.startErr[iface] = err
}

func (m *testDHCPMgr) preAttached(iface string) {
	m.getMap[iface] = &mockDHCPClient{}
}

type mockDHCPClient struct{}

func (m *mockDHCPClient) Start() error      { return nil }
func (m *mockDHCPClient) Stop()             {}
func (m *mockDHCPClient) CurrentIP() net.IP { return nil }
func (m *mockDHCPClient) IsAcquired() bool  { return false }

type testFWMgr struct {
	mu              sync.Mutex
	addCalls        []fwAddCall
	addServiceCalls []fwSvcCall
	removeCalls     []string
	addErr          map[string]error
}

type fwAddCall struct {
	iface      string
	extIP      string
	extPort    int
	protocol   string
	podIPs     []string
	podPort    int
	serviceKey string
}

type fwSvcCall struct {
	iface      string
	extIP      string
	ports      []firewall.PortRule
	serviceKey string
}

func newTestFWMgr() *testFWMgr {
	return &testFWMgr{addErr: make(map[string]error)}
}

func (m *testFWMgr) AddServiceRules(iface, extIP string, ports []firewall.PortRule, serviceKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// copy port slice to avoid retention of caller slice
	copied := make([]firewall.PortRule, len(ports))
	copy(copied, ports)
	m.addServiceCalls = append(m.addServiceCalls, fwSvcCall{iface, extIP, copied, serviceKey})
	return m.addErr[serviceKey]
}

func (m *testFWMgr) RemoveRules(serviceKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeCalls = append(m.removeCalls, serviceKey)
	return nil
}

func (m *testFWMgr) setAddErr(serviceKey string, err error) {
	m.addErr[serviceKey] = err
}

func TestRun_NoServices(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("eth0")
	linkMgr := newTestLinkMgr()
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(linkMgr.createCalls) != 0 {
		t.Errorf("expected no create calls, got %d", len(linkMgr.createCalls))
	}
}

func TestRun_RecoverSingleService(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   mac.String(),
		Interface: "lb-default-myapp",
		Ports: []state.PortEntry{
			{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.42.0.10"}},
		},
		CurrentIP:    "81.23.45.67",
		DHCPAcquired: true,
	}

	linkMgr := newTestLinkMgr()
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(linkMgr.createCalls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(linkMgr.createCalls))
	}
	call := linkMgr.createCalls[0]
	if call.iface != "lb-default-myapp" {
		t.Errorf("expected iface lb-default-myapp, got %q", call.iface)
	}
	if call.parent != "wan.201" {
		t.Errorf("expected parent wan.201, got %q", call.parent)
	}
	if call.mac.String() != mac.String() {
		t.Errorf("expected MAC %s, got %s", mac.String(), call.mac.String())
	}

	if len(linkMgr.setUpCalls) != 1 {
		t.Fatalf("expected 1 setUp call, got %d", len(linkMgr.setUpCalls))
	}
	if linkMgr.setUpCalls[0] != "lb-default-myapp" {
		t.Errorf("expected setUp lb-default-myapp, got %q", linkMgr.setUpCalls[0])
	}

	if len(dhcpMgr.newCalls) != 1 {
		t.Fatalf("expected 1 DHCP NewAndAdd call, got %d", len(dhcpMgr.newCalls))
	}
	if dhcpMgr.newCalls[0].iface != "lb-default-myapp" {
		t.Errorf("expected DHCP iface lb-default-myapp, got %q", dhcpMgr.newCalls[0].iface)
	}

	if len(fwMgr.addServiceCalls) != 1 {
		t.Fatalf("expected 1 firewall AddServiceRules call, got %d", len(fwMgr.addServiceCalls))
	}
	svc := fwMgr.addServiceCalls[0]
	if len(svc.ports) != 1 {
		t.Fatalf("expected 1 port in service call, got %d", len(svc.ports))
	}
	p := svc.ports[0]
	if p.ExtPort != 80 || p.PodPort != 8080 {
		t.Errorf("expected ports 80->8080, got %d->%d", p.ExtPort, p.PodPort)
	}
	if p.Protocol != "tcp" {
		t.Errorf("expected tcp, got %q", p.Protocol)
	}
}

func TestRun_SkipsCreateWhenInterfaceExists(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   mac.String(),
		Interface: "lb-default-myapp",
		Ports:     []state.PortEntry{},
	}

	linkMgr := newTestLinkMgr()
	linkMgr.preExists("lb-default-myapp", mac)
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(linkMgr.createCalls) != 0 {
		t.Errorf("expected no create calls when interface exists, got %d", len(linkMgr.createCalls))
	}

	if len(linkMgr.setUpCalls) != 1 {
		t.Errorf("expected 1 setUp call, got %d", len(linkMgr.setUpCalls))
	}
}

func TestRun_RecreatesInterfaceOnMACMismatch(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	expectedMAC := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	wrongMAC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xFF}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   expectedMAC.String(),
		Interface: "lb-default-myapp",
		Ports:     []state.PortEntry{},
	}

	linkMgr := newTestLinkMgr()
	linkMgr.preExists("lb-default-myapp", wrongMAC)
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(linkMgr.deleteCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(linkMgr.deleteCalls))
	}
	if linkMgr.deleteCalls[0] != "lb-default-myapp" {
		t.Errorf("expected delete lb-default-myapp, got %q", linkMgr.deleteCalls[0])
	}

	if len(linkMgr.createCalls) != 1 {
		t.Fatalf("expected 1 create call after delete, got %d", len(linkMgr.createCalls))
	}
}

func TestRun_RecreatesFirewallRules(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   mac.String(),
		Interface: "lb-default-myapp",
		Ports: []state.PortEntry{
			{Protocol: "tcp", ExternalPort: 443, PodPort: 8443, PodIPs: []string{"10.42.0.10", "10.42.0.11"}},
			{Protocol: "udp", ExternalPort: 53, PodPort: 5353, PodIPs: []string{"10.42.0.10"}},
		},
	}

	linkMgr := newTestLinkMgr()
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fwMgr.addServiceCalls) != 1 {
		t.Fatalf("expected 1 firewall AddServiceRules call, got %d", len(fwMgr.addServiceCalls))
	}
	svc := fwMgr.addServiceCalls[0]
	if len(svc.ports) != 2 {
		t.Fatalf("expected 2 ports in service call, got %d", len(svc.ports))
	}
	if svc.ports[0].Protocol != "tcp" || svc.ports[0].ExtPort != 443 {
		t.Errorf("first rule mismatch: %+v", svc.ports[0])
	}
	if svc.ports[1].Protocol != "udp" || svc.ports[1].ExtPort != 53 {
		t.Errorf("second rule mismatch: %+v", svc.ports[1])
	}
}

func TestRun_SkipsDHCPWhenAlreadyAttached(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   mac.String(),
		Interface: "lb-default-myapp",
		Ports:     []state.PortEntry{},
	}

	linkMgr := newTestLinkMgr()
	linkMgr.preExists("lb-default-myapp", mac)
	dhcpMgr := newTestDHCPMgr()
	dhcpMgr.preAttached("lb-default-myapp")
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(dhcpMgr.newCalls) != 0 {
		t.Errorf("expected no DHCP NewAndAdd when already attached, got %d", len(dhcpMgr.newCalls))
	}
}

func TestRun_FailsOnInvalidMAC(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   "not-a-valid-mac",
		Interface: "lb-default-myapp",
		Ports:     []state.PortEntry{},
	}

	linkMgr := newTestLinkMgr()
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err == nil {
		t.Fatal("expected error for invalid MAC")
	}
}

func TestRun_FailsOnCreateError(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   mac.String(),
		Interface: "lb-default-myapp",
		Ports:     []state.PortEntry{},
	}

	linkMgr := newTestLinkMgr()
	linkMgr.setCreateErr("lb-default-myapp", net.ErrClosed)
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err == nil {
		t.Fatal("expected error on create failure")
	}
}

func TestRun_FailsOnDeleteError(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	expectedMAC := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	wrongMAC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xFF}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   expectedMAC.String(),
		Interface: "lb-default-myapp",
		Ports:     []state.PortEntry{},
	}

	linkMgr := newTestLinkMgr()
	linkMgr.preExists("lb-default-myapp", wrongMAC)
	linkMgr.setDeleteErr("lb-default-myapp", net.ErrClosed)
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err == nil {
		t.Fatal("expected error on delete failure")
	}
}

func TestRun_FirewallErrorLoggedButNotFatal(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   mac.String(),
		Interface: "lb-default-myapp",
		Ports: []state.PortEntry{
			{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIPs: []string{"10.42.0.10"}},
		},
	}

	linkMgr := newTestLinkMgr()
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()
	fwMgr.setAddErr("default/myapp", net.ErrClosed)

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("firewall errors should be logged but not fatal, got: %v", err)
	}
}

func TestRun_RecoversMultipleServices(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac1 := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	mac2 := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x02}
	st.Services["default/app1"] = &state.LBService{
		MACAddr:   mac1.String(),
		Interface: "lb-default-app1",
		Ports:     []state.PortEntry{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIPs: []string{"10.42.0.10"}}},
	}
	st.Services["default/app2"] = &state.LBService{
		MACAddr:   mac2.String(),
		Interface: "lb-default-app2",
		Ports:     []state.PortEntry{{Protocol: "tcp", ExternalPort: 443, PodPort: 443, PodIPs: []string{"10.42.0.11"}}},
	}

	linkMgr := newTestLinkMgr()
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(linkMgr.createCalls) != 2 {
		t.Fatalf("expected 2 create calls, got %d", len(linkMgr.createCalls))
	}
	if len(dhcpMgr.newCalls) != 2 {
		t.Fatalf("expected 2 DHCP calls, got %d", len(dhcpMgr.newCalls))
	}
	if len(fwMgr.addServiceCalls) != 2 {
		t.Fatalf("expected 2 firewall service calls, got %d", len(fwMgr.addServiceCalls))
	}
}

func TestRun_PassesCurrentIPToFirewall(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   mac.String(),
		Interface: "lb-default-myapp",
		Ports:     []state.PortEntry{{Protocol: "tcp", ExternalPort: 80, PodPort: 80, PodIPs: []string{"10.42.0.10"}}},
		CurrentIP: "81.23.45.67",
	}

	linkMgr := newTestLinkMgr()
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fwMgr.addServiceCalls[0].extIP != "81.23.45.67" {
		t.Errorf("expected extIP 81.23.45.67, got %q", fwMgr.addServiceCalls[0].extIP)
	}
}

func TestRun_StartsDHCPClient(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   mac.String(),
		Interface: "lb-default-myapp",
		Ports:     []state.PortEntry{},
	}

	linkMgr := newTestLinkMgr()
	dhcpMgr := newTestDHCPMgr()
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(dhcpMgr.startCalls) != 1 {
		t.Fatalf("expected 1 DHCP Start call, got %d", len(dhcpMgr.startCalls))
	}
	if dhcpMgr.startCalls[0] != "lb-default-myapp" {
		t.Errorf("expected DHCP Start for lb-default-myapp, got %q", dhcpMgr.startCalls[0])
	}
}

func TestRun_StartsDHCPClientEvenWhenAlreadyAttached(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   mac.String(),
		Interface: "lb-default-myapp",
		Ports:     []state.PortEntry{},
	}

	linkMgr := newTestLinkMgr()
	linkMgr.preExists("lb-default-myapp", mac)
	dhcpMgr := newTestDHCPMgr()
	dhcpMgr.preAttached("lb-default-myapp")
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(dhcpMgr.startCalls) != 1 {
		t.Fatalf("expected 1 DHCP Start call for pre-attached client, got %d", len(dhcpMgr.startCalls))
	}
}

func TestRun_FailsOnDHCPStartError(t *testing.T) {
	logger := newTestLogger()
	st := state.NewState("wan.201")
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x00, 0x00, 0x01}
	st.Services["default/myapp"] = &state.LBService{
		MACAddr:   mac.String(),
		Interface: "lb-default-myapp",
		Ports:     []state.PortEntry{},
	}

	linkMgr := newTestLinkMgr()
	dhcpMgr := newTestDHCPMgr()
	dhcpMgr.setStartErr("lb-default-myapp", net.ErrClosed)
	fwMgr := newTestFWMgr()

	err := Run(logger, st, linkMgr, dhcpMgr, fwMgr)
	if err == nil {
		t.Fatal("expected error on DHCP Start failure")
	}
}
