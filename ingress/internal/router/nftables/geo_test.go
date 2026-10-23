//go:build linux

package nftables

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeSetStore struct {
	sets     map[string]*nftables.Set
	elements map[string][]nftables.SetElement
	added    []string
	flushed  []string
	deleted  []string
}

func newFakeSetStore() *fakeSetStore {
	return &fakeSetStore{
		sets:     make(map[string]*nftables.Set),
		elements: make(map[string][]nftables.SetElement),
	}
}

func (f *fakeSetStore) GetSets(*nftables.Table) ([]*nftables.Set, error) {
	out := make([]*nftables.Set, 0, len(f.sets))
	for _, s := range f.sets {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeSetStore) AddSet(s *nftables.Set, elements []nftables.SetElement) error {
	s.ID = 42
	f.sets[s.Name] = s
	f.elements[s.Name] = append([]nftables.SetElement(nil), elements...)
	f.added = append(f.added, s.Name)
	return nil
}

func (f *fakeSetStore) FlushSet(s *nftables.Set) { f.flushed = append(f.flushed, s.Name) }

func (f *fakeSetStore) SetAddElements(s *nftables.Set, elements []nftables.SetElement) error {
	f.elements[s.Name] = append([]nftables.SetElement(nil), elements...)
	return nil
}

func (f *fakeSetStore) DelSet(s *nftables.Set) {
	delete(f.sets, s.Name)
	delete(f.elements, s.Name)
	f.deleted = append(f.deleted, s.Name)
}

func TestValidateService_GeoCodes(t *testing.T) {
	tests := []struct {
		name string
		geos []string
		ok   bool
	}{
		{"empty", nil, true},
		{"valid pair", []string{"US", "CA"}, true},
		{"lowercase rejected", []string{"us"}, false},
		{"alpha-3 rejected", []string{"USA"}, false},
		{"one letter rejected", []string{"U"}, false},
		{"empty string rejected", []string{""}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateService(state.LBService{
				ServiceName: "ns/svc",
				Targets:     validTargets(),
				AllowedGeos: tc.geos,
			})
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestGeoSetName(t *testing.T) {
	sn := safeName("ns/svc")
	if got := geoSetName(sn); got != geoSetPrefix+sn {
		t.Fatalf("geo set name = %q", got)
	}
}

func TestGeoSetElements(t *testing.T) {
	cidrs := []*net.IPNet{
		mustCIDR(t, "192.0.2.1/32"),
		mustCIDR(t, "10.0.1.0/24"), // adjacent to the next range
		mustCIDR(t, "10.0.0.0/24"),
		mustCIDR(t, "10.0.0.0/25"), // overlapping subset
	}
	got := geoSetElements(cidrs)

	if len(got) != 3 {
		t.Fatalf("elements = %d, want 3: %+v", len(got), got)
	}
	assertKey(t, got[0].Key, 0x0a000000) // 10.0.0.0
	if got[0].IntervalEnd {
		t.Fatal("range start must not be an interval end")
	}
	assertKey(t, got[1].Key, 0x0a0001ff) // 10.0.1.255
	if !got[1].IntervalEnd {
		t.Fatal("range end must be an interval end")
	}
	assertKey(t, got[2].Key, 0xc0000201) // 192.0.2.1
	if got[2].IntervalEnd {
		t.Fatal("/32 must be a point element")
	}
}

func TestGeoSetElements_EmptyAndIPv6(t *testing.T) {
	if got := geoSetElements(nil); len(got) != 0 {
		t.Fatalf("nil CIDRs = %v", got)
	}
	_, v6, _ := net.ParseCIDR("fd00::/64")
	if got := geoSetElements([]*net.IPNet{v6}); len(got) != 0 {
		t.Fatalf("IPv6 must be ignored, got %v", got)
	}
}

func TestBuildGeoUnionSet_CreateThenFlush(t *testing.T) {
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	sn := safeName("ns/svc")

	set, err := buildGeoUnionSet(ss, table, sn, []*net.IPNet{mustCIDR(t, "10.44.0.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	if set.Name != geoSetName(sn) {
		t.Fatalf("set name = %q", set.Name)
	}
	if set.ID == 0 {
		t.Fatal("set ID not populated")
	}
	if !set.Interval || set.KeyType != nftables.TypeIPAddr {
		t.Fatalf("unexpected set shape: %+v", set)
	}
	if len(ss.added) != 1 || len(ss.elements[set.Name]) != 2 {
		t.Fatalf("added=%v elements=%v", ss.added, ss.elements[set.Name])
	}

	// Refresh with a different range: flush + repopulate, no second AddSet.
	_, err = buildGeoUnionSet(ss, table, sn, []*net.IPNet{mustCIDR(t, "10.45.0.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss.added) != 1 {
		t.Fatalf("set was re-added instead of flushed: %v", ss.added)
	}
	if len(ss.flushed) != 1 || ss.flushed[0] != set.Name {
		t.Fatalf("flushed = %v", ss.flushed)
	}
	if got := ss.elements[set.Name]; len(got) != 2 {
		t.Fatalf("refreshed elements = %v", got)
	}
}

func TestBuildGeoSetDenyExprs(t *testing.T) {
	set := &nftables.Set{Name: "geo_x", ID: 7}
	exprs := buildGeoSetDenyExprs(net.ParseIP("10.44.0.50"), set)

	if len(exprs) != 6 {
		t.Fatalf("expr count = %d", len(exprs))
	}
	payload, ok := exprs[0].(*expr.Payload)
	if !ok || payload.Offset != 0x10 {
		t.Fatalf("first expr is not the daddr payload: %#v", exprs[0])
	}
	cmp, ok := exprs[1].(*expr.Cmp)
	if !ok || cmp.Data == nil {
		t.Fatalf("second expr is not the extIP compare: %#v", exprs[1])
	}
	src, ok := exprs[2].(*expr.Payload)
	if !ok || src.Offset != 0x0c {
		t.Fatalf("third expr is not the saddr payload: %#v", exprs[2])
	}
	lookup, ok := exprs[3].(*expr.Lookup)
	if !ok || !lookup.Invert || lookup.SetID != 7 || lookup.SetName != "geo_x" {
		t.Fatalf("fourth expr is not an inverted set lookup: %#v", exprs[3])
	}
	if _, ok := exprs[4].(*expr.Counter); !ok {
		t.Fatalf("counter missing: %#v", exprs[4])
	}
	verdict, ok := exprs[5].(*expr.Verdict)
	if !ok || verdict.Kind != expr.VerdictDrop {
		t.Fatalf("verdict is not drop: %#v", exprs[5])
	}
}

func TestPlanRules_GeoDenyBeforeDNAT(t *testing.T) {
	m := &Manager{
		conn:      newTestConn(t),
		logger:    testLogger(),
		geoLookup: staticLookup(map[string][]string{"US": {"10.44.0.0/26"}, "CA": {"10.44.0.64/26"}}),
	}
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_x", Table: table}

	svc := state.LBService{
		ServiceName:   "ns/svc",
		SourceAddress: net.ParseIP("10.44.0.50"),
		AllowedGeos:   []string{"US", "CA"},
		Targets:       validTargets(),
	}

	rules, err := m.planRules(m.conn, ss, table, chain, svc)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("expected geo + dnat + drop, got %d rules", len(rules))
	}
	if !hasInvertedLookup(rules[0].Exprs) {
		t.Fatalf("first rule is not the geo deny: %#v", rules[0].Exprs)
	}
	if !hasDNAT(rules[1].Exprs) {
		t.Fatalf("second rule is not DNAT: %#v", rules[1].Exprs)
	}
	if !isPlainDrop(rules[2].Exprs) {
		t.Fatalf("last rule is not the final drop: %#v", rules[2].Exprs)
	}

	set := ss.sets[geoSetName(safeName("ns/svc"))]
	if set == nil {
		t.Fatal("geo set was not created")
	}
	// US and CA are adjacent in this fixture, so the union merges into one
	// range: start + interval end.
	if len(ss.elements[set.Name]) != 2 {
		t.Fatalf("union elements = %v", ss.elements[set.Name])
	}
	lookup := findLookup(rules[0].Exprs)
	if lookup.SetID != set.ID {
		t.Fatalf("deny rule references set ID %d, set has %d", lookup.SetID, set.ID)
	}
}

func TestPlanRules_NoGeosIsBackwardCompatible(t *testing.T) {
	m := &Manager{conn: newTestConn(t), logger: testLogger()}
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_x", Table: table}

	rules, err := m.planRules(m.conn, ss, table, chain, state.LBService{
		ServiceName:   "ns/svc",
		SourceAddress: net.ParseIP("10.44.0.50"),
		Targets:       validTargets(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected dnat + drop, got %d rules", len(rules))
	}
	if hasInvertedLookup(rules[0].Exprs) {
		t.Fatal("geo deny rule emitted without AllowedGeos")
	}
	if !hasDNAT(rules[0].Exprs) || !isPlainDrop(rules[1].Exprs) {
		t.Fatal("unexpected rule order without geos")
	}
	if len(ss.added) != 0 {
		t.Fatalf("geo set created without AllowedGeos: %v", ss.added)
	}
}

func TestPlanRules_GeosWithoutLookupFails(t *testing.T) {
	m := &Manager{conn: newTestConn(t), logger: testLogger()}
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_x", Table: table}

	_, err := m.planRules(m.conn, newFakeSetStore(), table, chain, state.LBService{
		ServiceName:   "ns/svc",
		SourceAddress: net.ParseIP("10.44.0.50"),
		AllowedGeos:   []string{"US"},
		Targets:       validTargets(),
	})
	if err == nil {
		t.Fatal("expected an error when MaxMind is not configured")
	}
}

func TestPlanRules_UnknownCountryFailsClosed(t *testing.T) {
	m := &Manager{
		conn:      newTestConn(t),
		logger:    testLogger(),
		geoLookup: staticLookup(nil),
	}
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_x", Table: table}

	rules, err := m.planRules(m.conn, ss, table, chain, state.LBService{
		ServiceName:   "ns/svc",
		SourceAddress: net.ParseIP("10.44.0.50"),
		AllowedGeos:   []string{"ZZ"},
		Targets:       validTargets(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasInvertedLookup(rules[0].Exprs) {
		t.Fatal("deny rule missing; empty allow-list must fail closed")
	}
	set := ss.sets[geoSetName(safeName("ns/svc"))]
	if set == nil {
		t.Fatal("empty geo set not created")
	}
	if len(ss.elements[set.Name]) != 0 {
		t.Fatalf("expected empty set, got %v", ss.elements[set.Name])
	}
}

func TestRefreshGeoSets_FlushesAndRepopulates(t *testing.T) {
	m := &Manager{logger: testLogger()}
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}

	svcCIDRs := map[string][]*net.IPNet{
		"ns/a": {mustCIDR(t, "10.44.0.0/26")},
	}
	if err := m.refreshGeoSets(ss, table, svcCIDRs); err != nil {
		t.Fatal(err)
	}
	name := geoSetName(safeName("ns/a"))
	if len(ss.added) != 1 || len(ss.elements[name]) != 2 {
		t.Fatalf("first pass: added=%v elements=%v", ss.added, ss.elements[name])
	}

	svcCIDRs["ns/a"] = []*net.IPNet{mustCIDR(t, "10.45.0.0/24")}
	if err := m.refreshGeoSets(ss, table, svcCIDRs); err != nil {
		t.Fatal(err)
	}
	if len(ss.added) != 1 {
		t.Fatalf("set re-added instead of flushed: %v", ss.added)
	}
	if len(ss.flushed) != 1 || ss.flushed[0] != name {
		t.Fatalf("flushed = %v", ss.flushed)
	}
	if got := ss.elements[name]; len(got) != 2 || !got[1].IntervalEnd {
		t.Fatalf("refreshed elements = %v", got)
	}
}

// -- helpers --

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func staticLookup(m map[string][]string) GeoLookup {
	return func(cc string) ([]*net.IPNet, bool) {
		cidrs, ok := m[cc]
		if !ok {
			return nil, false
		}
		var out []*net.IPNet
		for _, c := range cidrs {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				continue
			}
			out = append(out, n)
		}
		return out, true
	}
}

func assertKey(t *testing.T, key []byte, want uint32) {
	t.Helper()
	if len(key) != 4 {
		t.Fatalf("key length = %d: %v", len(key), key)
	}
	if got := binary.BigEndian.Uint32(key); got != want {
		t.Fatalf("key = %#x, want %#x", got, want)
	}
}

func findLookup(exprs []expr.Any) *expr.Lookup {
	for _, e := range exprs {
		if l, ok := e.(*expr.Lookup); ok {
			return l
		}
	}
	return nil
}

func hasInvertedLookup(exprs []expr.Any) bool {
	l := findLookup(exprs)
	return l != nil && l.Invert
}

func hasDNAT(exprs []expr.Any) bool {
	for _, e := range exprs {
		if _, ok := e.(*expr.NAT); ok {
			return true
		}
	}
	return false
}

func isPlainDrop(exprs []expr.Any) bool {
	if findLookup(exprs) != nil {
		return false
	}
	for _, e := range exprs {
		if v, ok := e.(*expr.Verdict); ok && v.Kind == expr.VerdictDrop {
			return true
		}
	}
	return false
}
