//go:build linux

package dhcp

// No t.Parallel() in this package: tests swap the process-global
// newRealClientFn seam, so they serialize on seamMu and restore via
// t.Cleanup instead.

import (
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

var seamMu sync.Mutex

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustTempStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"), testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestManager(t *testing.T) (*Manager, *state.State) {
	t.Helper()
	st := state.NewState("hl-br0", mustTempStore(t), testLogger())
	return NewManager(st, testLogger()), st
}

func seedService(t *testing.T, st *state.State, key, iface, macStr string) state.LBService {
	t.Helper()
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	svc := state.LBService{
		ServiceName: key,
		MACAddr:     mac,
		Targets:     []state.LBTarget{{Protocol: "tcp", SrcPort: 80}},
	}
	if iface != "" {
		svc.SetInterfaceName(iface)
	}
	st.UpdateService(&svc)
	return svc
}

// fakeClient implements Client with no network: every method only records.
type fakeClient struct {
	mu       sync.Mutex
	ip       net.IP
	mask     net.IPMask
	info     *LeaseInfo
	started  int
	stopped  int
	startErr error
}

func (f *fakeClient) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	return f.startErr
}

func (f *fakeClient) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
}

func (f *fakeClient) CurrentIP() net.IP {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ip
}

func (f *fakeClient) IsAcquired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ip != nil
}

func (f *fakeClient) LeaseInfo() *LeaseInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info
}

func (f *fakeClient) counts() (started, stopped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.stopped
}

// withFakeClient swaps the constructor seam to always return fake.
func withFakeClient(t *testing.T, fake *fakeClient) {
	t.Helper()
	seamMu.Lock()
	old := newRealClientFn
	newRealClientFn = func(string, net.HardwareAddr, *LeaseInfo, IPChangeFunc, *slog.Logger) Client {
		return fake
	}
	t.Cleanup(func() {
		newRealClientFn = old
		seamMu.Unlock()
	})
}

// TestNewRealClientFnDefaultIsRealConstructor proves the seam defaults to
// the real constructor: constructing through it is side-effect free (no
// network, no goroutine until Start) and yields an unacquired client.
func TestNewRealClientFnDefaultIsRealConstructor(t *testing.T) {
	if newRealClientFn == nil {
		t.Fatalf("newRealClientFn must default to the real constructor")
	}
	c := newRealClientFn("eth0", net.HardwareAddr{0x02, 0, 0, 0, 0, 1}, nil, nil, testLogger())
	if c == nil {
		t.Fatalf("default constructor returned nil")
	}
	if c.IsAcquired() || c.CurrentIP() != nil {
		t.Fatalf("fresh client must be unacquired")
	}
}

// TestManagerOnIPChangeWritesStateAddress pins the acquire path:
// acquire -> onIPChange -> UpdateService(SourceAddress/SourceMask,
// DHCPAcquired=true).
func TestManagerOnIPChangeWritesStateAddress(t *testing.T) {
	m, st := newTestManager(t)
	svc := seedService(t, st, "ns/svc", "eth0", "02:00:00:00:00:01")
	m.setServiceMapping(svc.ServiceName, "eth0")

	ip := net.ParseIP("10.44.0.55").To4()
	mask := net.CIDRMask(24, 32)
	m.onIPChange("eth0", ip, mask)

	got, ok := st.GetService("ns/svc")
	if !ok {
		t.Fatalf("service missing after onIPChange")
	}
	if !got.SourceAddress.Equal(ip) {
		t.Fatalf("SourceAddress = %s, want %s", got.SourceAddress, ip)
	}
	if !maskEqual(got.SourceMask, mask) {
		t.Fatalf("SourceMask = %s, want %s", got.SourceMask, mask)
	}
	if got.DHCPAcquired == nil || !*got.DHCPAcquired {
		t.Fatalf("DHCPAcquired must be true after acquire")
	}
}

// TestManagerOnIPChangeLeavesTargetsUntouched pins that only address state
// (plus the acquired flag) changes — never the desired backend set.
func TestManagerOnIPChangeLeavesTargetsUntouched(t *testing.T) {
	m, st := newTestManager(t)
	svc := seedService(t, st, "ns/svc", "eth0", "02:00:00:00:00:01")
	before := svc.Targets
	m.setServiceMapping(svc.ServiceName, "eth0")

	ch := st.Subscribe()
	defer st.Unsubscribe(ch)
	// Drain the create notification from seeding.
	<-ch

	sameIP := net.ParseIP("10.44.0.55").To4()
	sameMask := net.CIDRMask(24, 32)
	m.onIPChange("eth0", sameIP, sameMask) // first: applies
	<-ch

	// Second identical change must not re-notify (no UpdateService call).
	m.onIPChange("eth0", sameIP, sameMask)
	select {
	case n := <-ch:
		t.Fatalf("unexpected notification for identical IP/mask: %+v", n)
	case <-time.After(200 * time.Millisecond):
	}

	got, _ := st.GetService("ns/svc")
	if !reflect.DeepEqual(got.Targets, before) {
		t.Fatalf("Targets changed: %+v", got.Targets)
	}
}

// TestManagerEnsureAndStartDoubleStartIdempotent pins that repeated upserts
// for the same interface reuse the client instead of leaking Start calls.
func TestManagerEnsureAndStartDoubleStartIdempotent(t *testing.T) {
	m, st := newTestManager(t)
	fake := &fakeClient{}
	withFakeClient(t, fake)
	svc := seedService(t, st, "ns/svc", "eth0", "02:00:00:00:00:01")

	m.handleUpsert("eth0", svc)
	m.handleUpsert("eth0", svc)

	started, _ := fake.counts()
	if started != 1 {
		t.Fatalf("expected exactly 1 Start, got %d", started)
	}
	if m.serviceKeyToIface("ns/svc") != "eth0" {
		t.Fatalf("mapping must resolve to eth0")
	}

	m.StopAll()
	m.Stop()
	m.Stop() // idempotent: must not panic
	if _, stopped := fake.counts(); stopped != 1 {
		t.Fatalf("expected exactly 1 Stop, got %d", stopped)
	}
}

// TestManagerRenameRemovesOldClient pins rename-vs-delete: an interface
// rename stops the old client and moves the mapping (no stale resolution).
func TestManagerRenameRemovesOldClient(t *testing.T) {
	m, st := newTestManager(t)
	oldFake := &fakeClient{}
	withFakeClient(t, oldFake)
	svc := seedService(t, st, "ns/svc", "eth0", "02:00:00:00:00:01")
	m.handleUpsert("eth0", svc)

	newFake := &fakeClient{}
	seamMu.Lock()
	newRealClientFn = func(string, net.HardwareAddr, *LeaseInfo, IPChangeFunc, *slog.Logger) Client {
		return newFake
	}
	seamMu.Unlock()

	renamed := svc
	renamed.SetInterfaceName("eth1")
	st.UpdateService(&renamed)
	m.handleUpsert("eth1", renamed)

	if _, stopped := oldFake.counts(); stopped != 1 {
		t.Fatalf("old client must be stopped on rename")
	}
	if m.serviceKeyToIface("ns/svc") != "eth1" {
		t.Fatalf("mapping must resolve to eth1, got %q", m.serviceKeyToIface("ns/svc"))
	}
	if m.serviceKeyForInterface("eth0") != "" {
		t.Fatalf("stale eth0 mapping must be gone")
	}
}

// TestManagerDeleteRemovesClientAndLease pins delete: client stopped,
// mapping gone, persisted lease deleted.
func TestManagerDeleteRemovesClientAndLease(t *testing.T) {
	m, st := newTestManager(t)
	fake := &fakeClient{}
	withFakeClient(t, fake)
	svc := seedService(t, st, "ns/svc", "eth0", "02:00:00:00:00:01")
	m.handleUpsert("eth0", svc)
	if err := st.SaveLease("ns/svc", []byte(`{"service_name":"ns/svc"}`)); err != nil {
		t.Fatalf("SaveLease: %v", err)
	}

	m.handleDelete("eth0", "ns/svc")

	if _, stopped := fake.counts(); stopped != 1 {
		t.Fatalf("client must be stopped on delete")
	}
	if m.serviceKeyForInterface("eth0") != "" {
		t.Fatalf("mapping must be gone after delete")
	}
	leases, err := st.LoadLeases()
	if err != nil {
		t.Fatalf("LoadLeases: %v", err)
	}
	if _, ok := leases["ns/svc"]; ok {
		t.Fatalf("persisted lease must be deleted")
	}
}

// TestManagerLoadLeasesSkipsCorruptJSON pins the corrupt-lease path:
// Manager.loadLeases skips undecodable rows and still loads the good ones.
func TestManagerLoadLeasesSkipsCorruptJSON(t *testing.T) {
	store := mustTempStore(t)
	st := state.NewState("hl-br0", store, testLogger())
	if err := st.SaveLease("ns/good", []byte(`{"service_name":"ns/good"}`)); err != nil {
		t.Fatalf("SaveLease: %v", err)
	}
	if err := st.SaveLease("ns/bad", []byte(`{bad json`)); err != nil {
		t.Fatalf("SaveLease corrupt: %v", err)
	}

	m := NewManager(st, testLogger())
	m.mu.RLock()
	_, goodOK := m.leases["ns/good"]
	_, badOK := m.leases["ns/bad"]
	m.mu.RUnlock()
	if !goodOK {
		t.Fatalf("good lease must load")
	}
	if badOK {
		t.Fatalf("corrupt lease must be skipped")
	}
}

// TestManagerReconcileRemovesStaleClient pins the 30s reconcile's stale
// cleanup: a client whose service vanished from state (without handleDelete)
// is stopped, its lease deleted, and its mapping cleared.
func TestManagerReconcileRemovesStaleClient(t *testing.T) {
	m, st := newTestManager(t)
	fake := &fakeClient{}
	withFakeClient(t, fake)
	svc := seedService(t, st, "ns/svc", "eth0", "02:00:00:00:00:01")
	m.handleUpsert("eth0", svc)
	if err := st.SaveLease("ns/svc", []byte(`{"service_name":"ns/svc"}`)); err != nil {
		t.Fatalf("SaveLease: %v", err)
	}

	// Service vanishes from state without handleDelete (crash/simulated).
	st.DeleteService("ns/svc")
	m.reconcile()

	if _, stopped := fake.counts(); stopped != 1 {
		t.Fatalf("stale client must be stopped by reconcile")
	}
	if m.serviceKeyForInterface("eth0") != "" {
		t.Fatalf("stale mapping must be cleared by reconcile")
	}
}

// TestManagerPersistRestoreAcrossNewManager pins restart recovery: the
// lease written by persistLease is picked back up by a fresh Manager on
// the same DB file (fast rebind, no full Discover).
func TestManagerPersistRestoreAcrossNewManager(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	mkState := func() (*state.State, *state.Store) {
		s, err := state.NewStore(dbPath, testLogger())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return state.NewState("hl-br0", s, testLogger()), s
	}
	st1, _ := mkState()
	m1 := NewManager(st1, testLogger())

	ip := net.ParseIP("10.44.0.55").To4()
	mask := net.CIDRMask(24, 32)
	info := newLeaseInfo(&nclient4.Lease{ACK: mustACK(t, ip, mask)}, ip, mask)
	info.ServiceName = "ns/svc"
	fake := &fakeClient{ip: ip, mask: mask, info: info}
	m1.mu.Lock()
	m1.clients["eth0"] = fake
	m1.ifaceToService["eth0"] = "ns/svc"
	m1.mu.Unlock()
	m1.persistLease("eth0", "ns/svc")

	st2, _ := mkState()
	m2 := NewManager(st2, testLogger())
	m2.mu.RLock()
	restored, ok := m2.leases["ns/svc"]
	m2.mu.RUnlock()
	if !ok {
		t.Fatalf("lease must be restored by a fresh Manager")
	}
	if !restored.Address.Equal(ip) || restored.Mask.String() != mask.String() {
		t.Fatalf("restored lease mismatch: %+v", restored)
	}
	if restored.ServiceName != "ns/svc" {
		t.Fatalf("restored ServiceName = %q", restored.ServiceName)
	}
}

// mustACK builds a minimal valid ACK packet carrying the address.
func mustACK(t *testing.T, ip net.IP, mask net.IPMask) *dhcpv4.DHCPv4 {
	t.Helper()
	return syntheticACK(t, ip, mask, net.ParseIP("10.44.0.2").To4(), 24*time.Hour)
}

func TestMaskEqual(t *testing.T) {
	m24 := net.CIDRMask(24, 32)
	m25 := net.CIDRMask(25, 32)
	for _, tc := range []struct {
		name string
		a, b net.IPMask
		want bool
	}{
		{"nil nil", nil, nil, true},
		{"same", m24, net.CIDRMask(24, 32), true},
		{"different", m24, m25, false},
		{"different lengths", net.CIDRMask(24, 32), net.CIDRMask(24, 128), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskEqual(tc.a, tc.b); got != tc.want {
				t.Fatalf("maskEqual = %v, want %v", got, tc.want)
			}
		})
	}
}
