//go:build linux

package nftables

import (
	"net"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

func rateLimitedService(rl *state.RateLimit) state.LBService {
	return state.LBService{
		ServiceName:   "ns/svc",
		SourceAddress: net.ParseIP("10.44.0.50"),
		Targets:       validTargets(),
		RateLimit:     rl,
	}
}

func TestRateLimitSetName(t *testing.T) {
	sn := safeName("ns/svc")
	a := rateLimitSetName(sn, state.RateLimit{Rate: 5, Burst: 10})
	if a != rateLimitSetName(sn, state.RateLimit{Rate: 5, Burst: 10}) {
		t.Fatal("set name is not deterministic")
	}
	if a == rateLimitSetName(sn, state.RateLimit{Rate: 6, Burst: 10}) {
		t.Fatal("set name must change when the rate changes")
	}
	if a == rateLimitSetName(sn, state.RateLimit{Rate: 5, Burst: 11}) {
		t.Fatal("set name must change when the burst changes")
	}
	if got := rateLimitSetName(sn, state.RateLimit{Rate: 5, Burst: 10}); len(got) <= len(sn) {
		t.Fatalf("unexpected set name %q", got)
	}
}

func TestValidateService_RateLimit(t *testing.T) {
	tests := []struct {
		name string
		rl   *state.RateLimit
		ok   bool
	}{
		{"nil", nil, true},
		{"valid", &state.RateLimit{Rate: 5, Burst: 10}, true},
		{"zero rate", &state.RateLimit{Rate: 0, Burst: 10}, false},
		{"zero burst", &state.RateLimit{Rate: 5, Burst: 0}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateService(rateLimitedService(tc.rl))
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func findDynset(t *testing.T, exprs []expr.Any) *expr.Dynset {
	t.Helper()
	for _, e := range exprs {
		if d, ok := e.(*expr.Dynset); ok {
			return d
		}
	}
	return nil
}

func hasCtStateNew(exprs []expr.Any) bool {
	var ct bool
	var newBit bool
	for _, e := range exprs {
		switch v := e.(type) {
		case *expr.Ct:
			ct = ct || v.Key == expr.CtKeySTATE
		case *expr.Bitwise:
			newBit = newBit || (len(v.Mask) == 4 &&
				binaryutil.NativeEndian.Uint32(v.Mask) == expr.CtStateBitNEW)
		}
	}
	return ct && newBit
}

func hasCounterBeforeDrop(exprs []expr.Any) bool {
	counterAt := -1
	dropAt := -1
	for i, e := range exprs {
		switch v := e.(type) {
		case *expr.Counter:
			counterAt = i
		case *expr.Verdict:
			if v.Kind == expr.VerdictDrop {
				dropAt = i
			}
		}
	}
	return counterAt >= 0 && dropAt > counterAt
}

func TestPlanRules_RateLimit(t *testing.T) {
	m := &Manager{logger: testLogger()}
	c := newTestConn(t)
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_test", Table: table}

	rl := &state.RateLimit{Rate: 5, Burst: 10}
	rules, err := m.planRules(c, ss, table, chain, rateLimitedService(rl))
	if err != nil {
		t.Fatalf("planRules: %v", err)
	}
	// rate limit, DNAT, final drop.
	if len(rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(rules))
	}

	first := rules[0].Exprs
	d := findDynset(t, first)
	if d == nil {
		t.Fatal("first rule has no Dynset expression")
	}
	if d.Operation != uint32(unix.NFT_DYNSET_OP_UPDATE) {
		t.Fatalf("dynset op = %d, want UPDATE", d.Operation)
	}
	if d.Invert {
		t.Fatal("dynset must not be inverted")
	}
	if d.Timeout != rateLimitSetTimeout {
		t.Fatalf("dynset timeout = %s, want %s", d.Timeout, rateLimitSetTimeout)
	}
	if len(d.Exprs) != 1 {
		t.Fatalf("dynset exprs = %d, want 1", len(d.Exprs))
	}
	lim, ok := d.Exprs[0].(*expr.Limit)
	if !ok {
		t.Fatalf("dynset expr is %T, want *expr.Limit", d.Exprs[0])
	}
	if !lim.Over {
		t.Fatal("limit must use Over=true so only excess traffic hits the drop")
	}
	if lim.Rate != 5 || lim.Burst != 10 {
		t.Fatalf("limit = %d/%d, want 5/10", lim.Rate, lim.Burst)
	}
	if lim.Unit != expr.LimitTimeSecond {
		t.Fatalf("limit unit = %d, want second", lim.Unit)
	}
	if lim.Type != expr.LimitTypePkts {
		t.Fatalf("limit type = %d, want packets", lim.Type)
	}
	if !hasCtStateNew(first) {
		t.Fatal("rate-limit rule must match ct state new")
	}
	if !hasCounterBeforeDrop(first) {
		t.Fatal("rate-limit drop must be preceded by a counter")
	}

	set, ok := ss.sets[d.SetName]
	if !ok {
		t.Fatalf("rate-limit set %q was not created", d.SetName)
	}
	if !set.Dynamic || !set.HasTimeout || set.Timeout != rateLimitSetTimeout {
		t.Fatalf("set flags = dynamic:%v timeout:%v/%s", set.Dynamic, set.HasTimeout, set.Timeout)
	}
	if set.KeyType != nftables.TypeIPAddr {
		t.Fatalf("set key type = %v, want IPAddr", set.KeyType)
	}
	if set.Size != rateLimitSetSize {
		t.Fatalf("set size = %d, want %d", set.Size, rateLimitSetSize)
	}
	if d.SetID != set.ID {
		t.Fatalf("dynset SetID = %d, set ID = %d", d.SetID, set.ID)
	}

	// DNAT still follows, and the final rule is the daddr drop.
	if findDynset(t, rules[1].Exprs) != nil {
		t.Fatal("DNAT rule must not carry the meter")
	}
	if !hasDrop(t, rules[len(rules)-1].Exprs) {
		t.Fatal("last rule must be the daddr drop")
	}
}

func hasDrop(t *testing.T, exprs []expr.Any) bool {
	t.Helper()
	for _, e := range exprs {
		if v, ok := e.(*expr.Verdict); ok && v.Kind == expr.VerdictDrop {
			return true
		}
	}
	return false
}

func TestPlanRules_RateLimitAfterGeo(t *testing.T) {
	m := &Manager{logger: testLogger(), geoLookup: func(cc string) ([]*net.IPNet, bool) {
		return []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}, true
	}}
	c := newTestConn(t)
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_test", Table: table}

	svc := rateLimitedService(&state.RateLimit{Rate: 5, Burst: 10})
	svc.AllowedGeos = []string{"US"}
	rules, err := m.planRules(c, ss, table, chain, svc)
	if err != nil {
		t.Fatalf("planRules: %v", err)
	}
	if len(rules) != 4 {
		t.Fatalf("rules = %d, want 4 (geo, rate, DNAT, drop)", len(rules))
	}
	if findDynset(t, rules[0].Exprs) != nil {
		t.Fatal("geo deny must come first")
	}
	if findDynset(t, rules[1].Exprs) == nil {
		t.Fatal("rate limit must follow the geo deny and precede DNAT")
	}
}

// TestPlanRules_KeepsOldRateLimitSet pins the ordering contract: planRules
// must not delete the previous set, because the old rule still references it.
// AddRules deletes it only after flushing the chain.
func TestPlanRules_KeepsOldRateLimitSet(t *testing.T) {
	m := &Manager{logger: testLogger()}
	c := newTestConn(t)
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_test", Table: table}
	sn := safeName("ns/svc")

	old := rateLimitSetName(sn, state.RateLimit{Rate: 1, Burst: 1})
	ss.sets[old] = &nftables.Set{Table: table, Name: old, KeyType: nftables.TypeIPAddr, Dynamic: true}

	rules, err := m.planRules(c, ss, table, chain, rateLimitedService(&state.RateLimit{Rate: 5, Burst: 10}))
	if err != nil {
		t.Fatalf("planRules: %v", err)
	}
	if _, ok := ss.sets[old]; !ok {
		t.Fatal("planRules deleted a set that the old rule may still reference")
	}
	d := findDynset(t, rules[0].Exprs)
	if _, ok := ss.sets[d.SetName]; !ok {
		t.Fatalf("new set %q missing", d.SetName)
	}
}

func TestRemoveStaleRateLimitSets(t *testing.T) {
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	sn := safeName("ns/svc")

	old := rateLimitSetName(sn, state.RateLimit{Rate: 1, Burst: 1})
	keep := rateLimitSetName(sn, state.RateLimit{Rate: 5, Burst: 10})
	other := rateLimitSetName(safeName("ns/other"), state.RateLimit{Rate: 1, Burst: 1})
	for _, name := range []string{old, keep, other} {
		ss.sets[name] = &nftables.Set{Table: table, Name: name, KeyType: nftables.TypeIPAddr, Dynamic: true}
	}

	if err := removeStaleRateLimitSets(ss, table, sn, keep); err != nil {
		t.Fatalf("removeStaleRateLimitSets: %v", err)
	}
	if _, ok := ss.sets[old]; ok {
		t.Fatal("stale set was not deleted")
	}
	if _, ok := ss.sets[keep]; !ok {
		t.Fatal("current set was deleted")
	}
	if _, ok := ss.sets[other]; !ok {
		t.Fatal("another service's set was deleted")
	}

	// Disabling the limit removes every set for the service.
	if err := removeStaleRateLimitSets(ss, table, sn, ""); err != nil {
		t.Fatalf("removeStaleRateLimitSets without keep: %v", err)
	}
	if _, ok := ss.sets[keep]; ok {
		t.Fatal("set survived disabling the limit")
	}
	if _, ok := ss.sets[other]; !ok {
		t.Fatal("another service's set was deleted")
	}
}

func TestPlanRules_ReusesRateLimitSet(t *testing.T) {
	m := &Manager{logger: testLogger()}
	c := newTestConn(t)
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_test", Table: table}
	svc := rateLimitedService(&state.RateLimit{Rate: 5, Burst: 10})

	for i := 0; i < 2; i++ {
		if _, err := m.planRules(c, ss, table, chain, svc); err != nil {
			t.Fatalf("planRules %d: %v", i, err)
		}
	}
	name := rateLimitSetName(safeName(svc.ServiceName), *svc.RateLimit)
	if len(ss.added) != 1 || ss.added[0] != name {
		t.Fatalf("set added %v, want exactly [%s]", ss.added, name)
	}
}
