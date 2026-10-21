package geo

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeSource struct {
	mu        sync.Mutex
	cidrs     map[string][]*net.IPNet
	reloads   int
	reloadErr error
	onReload  func()
}

func newFakeSource() *fakeSource {
	return &fakeSource{cidrs: make(map[string][]*net.IPNet)}
}

func (f *fakeSource) set(cc string, cidrs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var nets []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		nets = append(nets, n)
	}
	f.cidrs[cc] = nets
}

func (f *fakeSource) Reload() error {
	f.mu.Lock()
	f.reloads++
	err := f.reloadErr
	hook := f.onReload
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (f *fakeSource) CountryCIDRs(cc string) ([]*net.IPNet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cidrs[cc], nil
}

func (f *fakeSource) LookupCountry(net.IP) (string, bool) { return "", false }

func (f *fakeSource) reloadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reloads
}

func testState(t *testing.T, services map[string][]string) *state.State {
	t.Helper()
	st := state.NewState("eth0", nil, discardLogger())
	for key, geos := range services {
		st.UpdateService(&state.LBService{
			ServiceName: key,
			Targets:     []state.LBTarget{{Protocol: "tcp", SrcPort: 80}},
			AllowedGeos: geos,
		})
	}
	return st
}

func TestServiceCIDRs_ConcatenatesPerCountry(t *testing.T) {
	src := newFakeSource()
	src.set("US", "10.44.0.0/26", "10.45.0.0/24")
	src.set("CA", "10.44.0.64/26")
	st := testState(t, map[string][]string{
		"ns/us-ca": {"US", "CA"},
		"ns/us":    {"US"},
		"ns/open":  nil,
	})
	m := NewManager(src, st, discardLogger(), "unused")

	got := m.ServiceCIDRs()
	if len(got) != 2 {
		t.Fatalf("expected 2 geo services, got %v", got)
	}
	if len(got["ns/us-ca"]) != 3 {
		t.Fatalf("us-ca union = %v", got["ns/us-ca"])
	}
	if got["ns/us-ca"][0].String() != "10.44.0.0/26" || got["ns/us-ca"][2].String() != "10.44.0.64/26" {
		t.Fatalf("unexpected order: %v", got["ns/us-ca"])
	}
	if len(got["ns/us"]) != 2 {
		t.Fatalf("us union = %v", got["ns/us"])
	}
	if _, ok := got["ns/open"]; ok {
		t.Fatal("service without AllowedGeos must not be in the map")
	}
}

func TestSnapshotInUse(t *testing.T) {
	st := testState(t, map[string][]string{
		"ns/a": {"US", "CA"},
		"ns/b": {"US"},
	})
	m := NewManager(newFakeSource(), st, discardLogger(), "unused")
	inUse := m.SnapshotInUse()
	if len(inUse) != 2 {
		t.Fatalf("in use = %v", inUse)
	}
	for _, cc := range []string{"US", "CA"} {
		if _, ok := inUse[cc]; !ok {
			t.Fatalf("%s missing from %v", cc, inUse)
		}
	}
}

func TestCountryCIDRs(t *testing.T) {
	src := newFakeSource()
	src.set("US", "10.44.0.0/26")
	m := NewManager(src, nil, discardLogger(), "unused")

	nets, ok := m.CountryCIDRs("us")
	if !ok || len(nets) != 1 {
		t.Fatalf("CountryCIDRs(us) = %v, %v", nets, ok)
	}
	if _, ok := m.CountryCIDRs("ZZ"); ok {
		t.Fatal("unknown country must report false")
	}
}

func TestReload_FiresRebuildCallback(t *testing.T) {
	src := newFakeSource()
	src.set("US", "10.44.0.0/26")
	st := testState(t, map[string][]string{"ns/a": {"US"}})
	m := NewManager(src, st, discardLogger(), filepath.Join(t.TempDir(), "geo.tsv"))

	var got map[string][]*net.IPNet
	m.OnRebuild(func(m map[string][]*net.IPNet) { got = m })

	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got["ns/a"]) != 1 {
		t.Fatalf("callback map = %v", got)
	}
}

func TestReload_ErrorSkipsCallback(t *testing.T) {
	src := newFakeSource()
	src.reloadErr = os.ErrInvalid
	m := NewManager(src, nil, discardLogger(), "unused")

	called := false
	m.OnRebuild(func(map[string][]*net.IPNet) { called = true })
	if err := m.Reload(); err == nil {
		t.Fatal("expected an error")
	}
	if called {
		t.Fatal("callback fired after a failed reload")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestRun_ReloadsOnAtomicRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "geo.tsv")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := newFakeSource()
	m := NewManager(src, nil, discardLogger(), path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitFor(t, 2*time.Second, func() bool { return src.reloadCount() >= 1 }, "initial reload did not run")

	tmp := filepath.Join(dir, "geo.new")
	if err := os.WriteFile(tmp, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool { return src.reloadCount() >= 2 }, "fsnotify rename did not trigger a reload")
}

func TestRun_FallbackPoller(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "geo.tsv")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := newFakeSource()
	m := NewManager(src, nil, discardLogger(), path, WithPollInterval(10*time.Millisecond))
	m.disableWatch = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitFor(t, 2*time.Second, func() bool { return src.reloadCount() >= 1 }, "initial reload did not run")

	if err := os.WriteFile(path, []byte("content changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return src.reloadCount() >= 2 }, "poller did not detect the change")
}

func TestRun_StopsOnCancel(t *testing.T) {
	src := newFakeSource()
	m := NewManager(src, nil, discardLogger(), filepath.Join(t.TempDir(), "geo.tsv"))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
