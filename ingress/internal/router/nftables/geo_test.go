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
	sets       map[string]*nftables.Set
	elements   map[string][]nftables.SetElement
	added      []string
	flushed    []string
	deleted    []string
	flushes    int
	chunkCalls int
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

func (f *fakeSetStore) FlushSet(s *nftables.Set) {
	f.flushed = append(f.flushed, s.Name)
	f.elements[s.Name] = nil
}

func (f *fakeSetStore) SetAddElements(s *nftables.Set, elements []nftables.SetElement) error {
	f.elements[s.Name] = append(f.elements[s.Name], elements...)
	f.chunkCalls++
	return nil
}

func (f *fakeSetStore) Flush() error {
	f.flushes++
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

func TestGeoSetChunksKeepRangesWhole(t *testing.T) {
	cidrs := make([]*net.IPNet, 0, 12)
	// Non-adjacent points plus multi-element ranges, so a naive element
	// slicer would split pairs at several boundaries.
	for i := 0; i < 8; i++ {
		v := uint32(10)<<24 | uint32(i*2)
		ip := net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
		cidrs = append(cidrs, &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)})
	}
	cidrs = append(cidrs,
		mustCIDR(t, "10.1.0.0/24"),
		mustCIDR(t, "10.2.0.0/16"),
		mustCIDR(t, "10.4.0.0/20"),
	)

	const max = 3
	chunks := geoSetChunks(cidrs, max)
	want := geoSetElements(cidrs)

	var flat []nftables.SetElement
	chunkOf := make([]int, 0, len(want))
	for ci, chunk := range chunks {
		if len(chunk) > max {
			t.Fatalf("chunk %d has %d elements, max %d", ci, len(chunk), max)
		}
		flat = append(flat, chunk...)
		for range chunk {
			chunkOf = append(chunkOf, ci)
		}
	}
	if len(flat) != len(want) {
		t.Fatalf("chunks flattened to %d elements, want %d", len(flat), len(want))
	}
	for i := range flat {
		if string(flat[i].Key) != string(want[i].Key) || flat[i].IntervalEnd != want[i].IntervalEnd {
			t.Fatalf("element %d = %+v, want %+v", i, flat[i], want[i])
		}
	}
	for i, el := range want {
		if !el.IntervalEnd {
			continue
		}
		if i == 0 {
			t.Fatalf("interval end at index 0 has no start")
		}
		if chunkOf[i] != chunkOf[i-1] {
			t.Fatalf("interval end at index %d is in chunk %d, its start in chunk %d",
				i, chunkOf[i], chunkOf[i-1])
		}
	}
}

func TestAddSetElementsChunked(t *testing.T) {
	ss := newFakeSetStore()
	set := &nftables.Set{Name: "chunked"}
	total := 2*geoSetChunkSize + 1
	els := make([]nftables.SetElement, 0, total)
	for i := 0; i < total; i++ {
		els = append(els, nftables.SetElement{Key: binary.BigEndian.AppendUint32(nil, uint32(i+1))})
	}

	if err := addSetElementsChunked(ss, set, els); err != nil {
		t.Fatalf("addSetElementsChunked: %v", err)
	}
	if ss.chunkCalls != 3 {
		t.Fatalf("chunk calls = %d, want 3", ss.chunkCalls)
	}
	if ss.flushes != 3 {
		t.Fatalf("flushes = %d, want 3", ss.flushes)
	}
	if got := len(ss.elements[set.Name]); got != total {
		t.Fatalf("elements = %d, want %d", got, total)
	}
}

// TestBuildGeoUnionSet_IntervalChunksKeepRangesWhole is the regression test
// for the EEXIST ("file exists") failure seen with large real geo sets after
// chunking was introduced: a flat element slice can split an interval range's
// start from its IntervalEnd, and the kernel rejects the orphan end.
//
// The synthetic set has 1,999 disjoint /32 ranges followed by one /24 range.
// A flat 2,000-element chunk boundary lands exactly between the /24 start and
// its IntervalEnd. It needs a writable netlink socket; unprivileged runs skip.
func TestBuildGeoUnionSet_IntervalChunksKeepRangesWhole(t *testing.T) {
	c, err := nftables.New()
	if err != nil {
		t.Skipf("nftables unavailable: %v", err)
	}

	table := &nftables.Table{Name: "hl_test_geo_chunks", Family: nftables.TableFamilyIPv4}
	if tables, err := c.ListTables(); err == nil {
		for _, old := range tables {
			if old.Name == table.Name {
				c.DelTable(old)
			}
		}
	}
	c.AddTable(table)
	if err := c.Flush(); err != nil {
		t.Skipf("nftables not writable: %v", err)
	}
	defer func() {
		c.DelTable(table)
		_ = c.Flush()
	}()

	// 1,999 non-adjacent /32s (step 2 so geoRanges does not merge them),
	// then a /24 whose start+end straddle the flat 2,000-element boundary.
	cidrs := make([]*net.IPNet, 0, 2000)
	for i := 0; i < 1999; i++ {
		v := uint32(10)<<24 | uint32(i*2)
		ip := net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
		cidrs = append(cidrs, &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)})
	}
	cidrs = append(cidrs, mustCIDR(t, "10.1.0.0/24"))

	set, err := buildGeoUnionSet(c, table, "testgeo", cidrs)
	if err != nil {
		t.Fatalf("buildGeoUnionSet: %v", err)
	}
	got, err := c.GetSetElements(set)
	if err != nil {
		t.Fatalf("GetSetElements: %v", err)
	}
	if len(got) != 1999*2+2 {
		t.Fatalf("set elements = %d, want %d", len(got), 1999*2+2)
	}
}

// TestAddSetElementsChunkedRealKernel is the regression test for the
// EMSGSIZE ("sendmsg: message too long") failure seen with real US+CA geo
// sets. It needs a writable netlink socket; unprivileged runs skip.
func TestAddSetElementsChunkedRealKernel(t *testing.T) {
	c, err := nftables.New()
	if err != nil {
		t.Skipf("nftables unavailable: %v", err)
	}

	table := &nftables.Table{Name: "hl_test_chunk", Family: nftables.TableFamilyIPv4}
	if tables, err := c.ListTables(); err == nil {
		for _, old := range tables {
			if old.Name == table.Name {
				c.DelTable(old)
			}
		}
	}
	c.AddTable(table)
	if err := c.Flush(); err != nil {
		t.Skipf("nftables not writable: %v", err)
	}
	defer func() {
		c.DelTable(table)
		_ = c.Flush()
	}()

	set := &nftables.Set{Table: table, Name: "s", KeyType: nftables.TypeIPAddr}
	if err := c.AddSet(set, nil); err != nil {
		t.Fatalf("AddSet: %v", err)
	}
	if err := c.Flush(); err != nil {
		t.Fatalf("create set: %v", err)
	}

	// 20k elements reproduces EMSGSIZE when sent as one message on the test
	// VM; chunking must keep each message under the socket limit.
	total := 20000
	els := make([]nftables.SetElement, 0, total)
	for i := 0; i < total; i++ {
		els = append(els, nftables.SetElement{Key: binary.BigEndian.AppendUint32(nil, uint32(i+1))})
	}
	if err := addSetElementsChunked(c, set, els); err != nil {
		t.Fatalf("addSetElementsChunked: %v", err)
	}

	got, err := c.GetSetElements(set)
	if err != nil {
		t.Fatalf("GetSetElements: %v", err)
	}
	if len(got) != total {
		t.Fatalf("set elements = %d, want %d", len(got), total)
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

	if len(got) != 4 {
		t.Fatalf("elements = %d, want 4: %+v", len(got), got)
	}
	assertKey(t, got[0].Key, 0x0a000000) // 10.0.0.0
	if got[0].IntervalEnd {
		t.Fatal("range start must not be an interval end")
	}
	// Interval ends are exclusive: [10.0.0.0, 10.0.1.255] ends at 10.0.2.0.
	assertKey(t, got[1].Key, 0x0a000200)
	if !got[1].IntervalEnd {
		t.Fatal("range end must be an interval end")
	}
	assertKey(t, got[2].Key, 0xc0000201) // 192.0.2.1
	if got[2].IntervalEnd {
		t.Fatal("point start must not be an interval end")
	}
	// A point 192.0.2.1 is [192.0.2.1, 192.0.2.2).
	assertKey(t, got[3].Key, 0xc0000202)
	if !got[3].IntervalEnd {
		t.Fatal("point end must be an interval end")
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

func TestValidateService_CIDRs(t *testing.T) {
	tests := []struct {
		name  string
		cidrs []string
		ok    bool
	}{
		{"nil", nil, true},
		{"valid pair", []string{"10.0.0.0/8", "192.168.1.0/24"}, true},
		{"host bits tolerated (masked to the network)", []string{"192.168.1.1/24"}, true},
		{"not a cidr rejected", []string{"bogus"}, false},
		{"bare IP rejected", []string{"10.0.0.1"}, false},
		{"ipv6 rejected", []string{"fd00::/64"}, false},
		{"mixed valid and invalid rejected", []string{"10.0.0.0/8", "bogus"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateService(state.LBService{
				ServiceName:  "ns/svc",
				Targets:      validTargets(),
				AllowedCIDRs: tc.cidrs,
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

func TestPlanRules_CIDRsOnlyWithoutLookup(t *testing.T) {
	// Static CIDRs need no country database: a nil geo lookup must not fail
	// a CIDR-only service.
	m := &Manager{conn: newTestConn(t), logger: testLogger()}
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_x", Table: table}

	rules, err := m.planRules(m.conn, ss, table, chain, state.LBService{
		ServiceName:   "ns/svc",
		SourceAddress: net.ParseIP("10.44.0.50"),
		AllowedCIDRs:  []string{"192.168.1.0/24"},
		Targets:       validTargets(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("expected geo + dnat + drop, got %d rules", len(rules))
	}
	if !hasInvertedLookup(rules[0].Exprs) {
		t.Fatalf("first rule is not the deny: %#v", rules[0].Exprs)
	}
	set := ss.sets[geoSetName(safeName("ns/svc"))]
	if set == nil {
		t.Fatal("geo set was not created")
	}
	if len(ss.elements[set.Name]) != 2 {
		t.Fatalf("static CIDR elements = %v", ss.elements[set.Name])
	}
}

func TestPlanRules_GeosAndCIDRsUnion(t *testing.T) {
	m := &Manager{
		conn:      newTestConn(t),
		logger:    testLogger(),
		geoLookup: staticLookup(map[string][]string{"US": {"10.44.0.0/26"}}),
	}
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_x", Table: table}

	rules, err := m.planRules(m.conn, ss, table, chain, state.LBService{
		ServiceName:   "ns/svc",
		SourceAddress: net.ParseIP("10.44.0.50"),
		AllowedGeos:   []string{"US"},
		AllowedCIDRs:  []string{"192.168.1.0/24"},
		Targets:       validTargets(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("expected geo + dnat + drop, got %d rules", len(rules))
	}
	if !hasInvertedLookup(rules[0].Exprs) {
		t.Fatalf("first rule is not the deny: %#v", rules[0].Exprs)
	}
	// Two disjoint ranges (country + static) → start + exclusive end each.
	set := ss.sets[geoSetName(safeName("ns/svc"))]
	if set == nil {
		t.Fatal("geo set was not created")
	}
	if len(ss.elements[set.Name]) != 4 {
		t.Fatalf("union elements = %v", ss.elements[set.Name])
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
