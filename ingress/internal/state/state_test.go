package state

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

func testState() *State {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// nil store: persistence is skipped, notifications still fire.
	return NewState("eth0", nil, logger)
}

func svcWithAddr(key, addr string) *LBService {
	return &LBService{
		ServiceName:   key,
		SourceAddress: net.ParseIP(addr),
		Targets:       []LBTarget{{Protocol: "tcp", SrcPort: 80}},
	}
}

func recvTimeout(t *testing.T, ch chan LBNotification) LBNotification {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for notification")
		return LBNotification{}
	}
}

func TestUpdateService_EmitsCreateThenUpdate(t *testing.T) {
	st := testState()
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	st.UpdateService(svcWithAddr("ns/svc", "10.44.0.50"))
	if n := recvTimeout(t, ch); n.Type != CreateEvent {
		t.Fatalf("expected create event, got %q", n.Type)
	}

	st.UpdateService(svcWithAddr("ns/svc", "10.44.0.51"))
	if n := recvTimeout(t, ch); n.Type != UpdateEvent {
		t.Fatalf("expected update event, got %q", n.Type)
	}

	if !st.HasService("ns/svc") {
		t.Fatalf("expected service to exist")
	}
	if got, ok := st.GetService("ns/svc"); !ok || got.SourceAddress.String() != "10.44.0.51" {
		t.Fatalf("expected updated address, got %+v", got)
	}
}

func TestDeleteService_EmitsDelete(t *testing.T) {
	st := testState()
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	st.UpdateService(svcWithAddr("ns/svc", "10.44.0.50"))
	recvTimeout(t, ch) // create

	st.DeleteService("ns/svc")
	if n := recvTimeout(t, ch); n.Type != DeleteEvent {
		t.Fatalf("expected delete event, got %q", n.Type)
	}
	if st.HasService("ns/svc") {
		t.Fatalf("expected service to be gone")
	}

	// Deleting a missing service is a no-op and emits nothing.
	st.DeleteService("ns/nope")
	select {
	case n := <-ch:
		t.Fatalf("unexpected notification: %+v", n)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestUnsubscribe_StopsDelivery(t *testing.T) {
	st := testState()
	ch := st.Subscribe()
	st.Unsubscribe(ch)

	// Unsubscribe closes the channel; a post-unsubscribe update must
	// neither panic (send on closed channel) nor deliver.
	st.UpdateService(svcWithAddr("ns/svc", "10.44.0.50"))
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("received a live notification after unsubscribe")
		}
		// closed channel: expected, no delivery.
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("channel not closed by unsubscribe")
	}
}

// TestClone_DeepCopy ensures mutating the original after Clone does not
// leak into the snapshot the reconcilers iterate over.
func TestClone_DeepCopy(t *testing.T) {
	st := testState()
	mac := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	iface := "lb-ns-svc-abc123"
	const wantIface = "lb-ns-svc-abc123"
	acquired := true
	const wantAcquired = true
	st.UpdateService(&LBService{
		ServiceName:   "ns/svc",
		Interface:     &iface,
		MACAddr:       mac,
		DHCPAcquired:  &acquired,
		SourceAddress: net.ParseIP("10.44.0.50"),
		SourceMask:    net.CIDRMask(24, 32),
		Targets: []LBTarget{{
			Protocol: "tcp",
			SrcPort:  80,
			Dst:      []LBDestination{{Port: 8080, Address: net.ParseIP("10.0.0.1")}},
		}},
	})

	snap := st.Clone()
	orig, _ := st.GetService("ns/svc")

	// Mutate every mutable field of the live object.
	*orig.Interface = "changed"
	*orig.DHCPAcquired = false
	orig.MACAddr[0] = 0xff
	orig.SourceAddress[0] = 0xff
	orig.SourceMask[0] = 0x00
	orig.Targets[0].Dst[0].Address[0] = 0xff
	orig.Targets[0].Dst[0].Port = 1

	got := snap.Services["ns/svc"]
	if *got.Interface != wantIface {
		t.Fatalf("interface leaked: %q", *got.Interface)
	}
	if *got.DHCPAcquired != wantAcquired {
		t.Fatalf("dhcp flag leaked")
	}
	if got.MACAddr[0] == 0xff {
		t.Fatalf("mac leaked")
	}
	if got.SourceAddress.String() != "10.44.0.50" {
		t.Fatalf("source address leaked: %s", got.SourceAddress)
	}
	if got.SourceMask.String() != "ffffff00" {
		t.Fatalf("source mask leaked: %s", got.SourceMask)
	}
	if got.Targets[0].Dst[0].Address.String() != "10.0.0.1" || got.Targets[0].Dst[0].Port != 8080 {
		t.Fatalf("destination leaked: %+v", got.Targets[0].Dst[0])
	}
}

// TestConcurrentSubscribeNotifyUnsubscribe churns subscribers and
// notifications from parallel goroutines. Run with -race: before the
// copy-on-remove + RLock-held notify fix, this exposed the slice-header
// data race and send-on-closed-channel panics from ws + gRPC/DHCP
// concurrency. It asserts nothing timing-dependent — only survival.
func TestConcurrentSubscribeNotifyUnsubscribe(t *testing.T) {
	st := testState()
	done := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ch := st.Subscribe()
				st.UpdateService(svcWithAddr("ns/svc", "10.44.0.50"))
				select {
				case <-ch:
				default:
				}
				st.Unsubscribe(ch)
				_ = n
			}
		}(g)
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("concurrent churn did not finish")
	}
}

func TestClone_NilService(t *testing.T) {
	var svc *LBService
	if svc.Clone() != nil {
		t.Fatalf("expected nil Clone of nil service")
	}
}

func TestParseMAC_RoundTrip(t *testing.T) {
	svc := &LBService{}
	mac := net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}
	svc.SetMAC(mac)
	got, err := svc.ParseMAC()
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	if got.String() != mac.String() {
		t.Fatalf("expected %s, got %s", mac, got)
	}
	if name := svc.GetInterfaceName(); name != "" {
		t.Fatalf("expected empty interface name, got %q", name)
	}
	svc.SetInterfaceName("lb-foo")
	if name := svc.GetInterfaceName(); name != "lb-foo" {
		t.Fatalf("expected lb-foo, got %q", name)
	}
}
