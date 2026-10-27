//go:build linux

package nftables

import (
	"net"
	"strings"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

func deniedService() state.LBService {
	return state.LBService{
		ServiceName:   "ns/svc",
		SourceAddress: net.ParseIP("10.44.0.50"),
		DeniedCIDRs:   []string{"203.0.113.0/24"},
		Targets:       validTargets(),
	}
}

func TestDenySetName(t *testing.T) {
	sn := safeName("ns/svc")
	name := denySetName(sn)
	if !strings.Contains(name, sn) {
		t.Fatalf("deny set name %q does not contain the service prefix %q (RemoveRules GC relies on this)", name, sn)
	}
	if strings.Contains(name, "/") || strings.Contains(name, "-") {
		t.Fatalf("deny set name %q contains nftables-hostile characters", name)
	}
}

func TestValidateService_DeniedCIDRs(t *testing.T) {
	tests := []struct {
		name   string
		denied []string
		ok     bool
	}{
		{"nil", nil, true},
		{"valid pair", []string{"203.0.113.0/24", "198.51.100.1/32"}, true},
		{"host bits tolerated", []string{"203.0.113.7/24"}, true},
		{"not a cidr rejected", []string{"bogus"}, false},
		{"bare IP rejected", []string{"203.0.113.1"}, false},
		{"ipv6 rejected", []string{"2001:db8::/32"}, false},
		{"mixed valid and invalid rejected", []string{"203.0.113.0/24", "bogus"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := deniedService()
			svc.DeniedCIDRs = tc.denied
			if err := validateService(svc); tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			} else if !tc.ok && err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestValidateService_LBPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy string
		ok     bool
	}{
		{"empty default", "", true},
		{"round-robin", "round-robin", true},
		{"hash-source-ip", "hash-source-ip", true},
		{"random", "random", true},
		{"least-conn rejected", "least-conn", false},
		{"uppercase rejected", "Random", false},
		{"blank rejected", "round robin", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := deniedService()
			svc.DeniedCIDRs = nil
			svc.LBPolicy = tc.policy
			if err := validateService(svc); tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			} else if !tc.ok && err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestPlanRules_DenyFirst(t *testing.T) {
	m := &Manager{
		conn:      newTestConn(t),
		logger:    testLogger(),
		geoLookup: staticLookup(map[string][]string{"US": {"10.44.0.0/26"}}),
	}
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_x", Table: table}

	svc := deniedService()
	svc.AllowedGeos = []string{"US"}
	svc.RateLimit = &state.RateLimit{Rate: 5, Burst: 10}

	rules, err := m.planRules(m.conn, ss, table, chain, svc)
	if err != nil {
		t.Fatal(err)
	}
	// deny, geo, rate-limit, DNAT, final drop.
	if len(rules) != 5 {
		t.Fatalf("rules = %d, want 5 (deny, geo, rate, dnat, drop)", len(rules))
	}

	deny := findLookup(rules[0].Exprs)
	if deny == nil || deny.Invert {
		t.Fatalf("first rule must be a non-inverted deny lookup: %#v", rules[0].Exprs)
	}
	sn := safeName("ns/svc")
	if deny.SetName != denySetName(sn) {
		t.Fatalf("deny set = %q, want %q", deny.SetName, denySetName(sn))
	}
	denySet := ss.sets[denySetName(sn)]
	if denySet == nil {
		t.Fatal("deny set was not created")
	}
	if deny.SetID != denySet.ID {
		t.Fatalf("deny rule references set ID %d, set has %d", deny.SetID, denySet.ID)
	}
	if len(ss.elements[denySet.Name]) != 2 {
		t.Fatalf("deny elements = %v", ss.elements[denySet.Name])
	}
	if !hasCounterBeforeDrop(rules[0].Exprs) {
		t.Fatal("deny drop must be preceded by a counter")
	}

	if !hasInvertedLookup(rules[1].Exprs) {
		t.Fatalf("second rule must be the geo deny: %#v", rules[1].Exprs)
	}
	if findDynset(t, rules[1].Exprs) != nil {
		t.Fatal("geo deny must not carry the meter")
	}
	if findDynset(t, rules[2].Exprs) == nil {
		t.Fatal("rate limit must follow the geo deny and precede DNAT")
	}
	if !hasDNAT(rules[3].Exprs) {
		t.Fatal("fourth rule must be DNAT")
	}
	if !isPlainDrop(rules[4].Exprs) {
		t.Fatal("last rule must be the final drop")
	}
}

func TestPlanRules_DenyWithoutGeoLookup(t *testing.T) {
	// Deny CIDRs are static: no country database is needed.
	m := &Manager{conn: newTestConn(t), logger: testLogger()}
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_x", Table: table}

	rules, err := m.planRules(m.conn, ss, table, chain, deniedService())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("rules = %d, want 3 (deny, dnat, drop)", len(rules))
	}
	deny := findLookup(rules[0].Exprs)
	if deny == nil || deny.Invert {
		t.Fatalf("first rule must be the deny: %#v", rules[0].Exprs)
	}
	if !hasDNAT(rules[1].Exprs) || !isPlainDrop(rules[2].Exprs) {
		t.Fatal("unexpected rule order without geos")
	}
}

func TestPlanRules_DenyFailsClosedOnInvalid(t *testing.T) {
	// validateService rejects these, so planRules is defensive: the invalid
	// entry is skipped and the deny rule stays in place with an empty set.
	m := &Manager{conn: newTestConn(t), logger: testLogger()}
	ss := newFakeSetStore()
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	chain := &nftables.Chain{Name: "pre_x", Table: table}

	svc := deniedService()
	svc.DeniedCIDRs = []string{"bogus"}
	rules, err := m.planRules(m.conn, ss, table, chain, svc)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("rules = %d, want 3 (deny, dnat, drop)", len(rules))
	}
	deny := findLookup(rules[0].Exprs)
	if deny == nil || deny.Invert {
		t.Fatal("deny rule missing; invalid input must fail closed")
	}
	set := ss.sets[denySetName(safeName("ns/svc"))]
	if set == nil || len(ss.elements[set.Name]) != 0 {
		t.Fatalf("expected an empty deny set, got %v", ss.elements)
	}
}

func twoBackends() map[string]uint16 {
	return map[string]uint16{"10.0.0.1": 8080, "10.0.0.2": 8080}
}

func TestBuildLBExprs_DefaultIsRoundRobin(t *testing.T) {
	c := newTestConn(t)
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	extIP := net.ParseIP("10.44.0.50")

	for _, policy := range []string{"", "round-robin"} {
		got, err := buildLBExprs(c, "sn", table, extIP, "tcp", 80, twoBackends(), policy)
		if err != nil {
			t.Fatalf("policy %q: %v", policy, err)
		}
		idx := findNumgen(got)
		if idx < 0 {
			t.Fatalf("policy %q: no numgen expression", policy)
		}
		ng := got[idx].(*expr.Numgen)
		if ng.Type != unix.NFT_NG_INCREMENTAL {
			t.Fatalf("policy %q: numgen type = %d, want incremental", policy, ng.Type)
		}
		if ng.Modulus != 2 {
			t.Fatalf("policy %q: modulus = %d, want 2", policy, ng.Modulus)
		}
	}
}

func TestBuildLBExprs_Random(t *testing.T) {
	c := newTestConn(t)
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	extIP := net.ParseIP("10.44.0.50")

	got, err := buildLBExprs(c, "sn", table, extIP, "tcp", 80, twoBackends(), "random")
	if err != nil {
		t.Fatal(err)
	}
	idx := findNumgen(got)
	if idx < 0 {
		t.Fatal("random pipeline has no numgen expression")
	}
	ng := got[idx].(*expr.Numgen)
	if ng.Type == unix.NFT_NG_INCREMENTAL {
		t.Fatal("random pipeline must not use the incremental numgen")
	}
	if ng.Modulus != 2 {
		t.Fatalf("modulus = %d, want 2", ng.Modulus)
	}
	if findLookup(got) == nil {
		t.Fatal("random pipeline lost its map lookup")
	}
	if !hasDNAT(got) {
		t.Fatal("random pipeline lost its DNAT")
	}
}

func TestBuildLBExprs_HashSourceIP(t *testing.T) {
	c := newTestConn(t)
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	extIP := net.ParseIP("10.44.0.50")

	got, err := buildLBExprs(c, "sn", table, extIP, "tcp", 80, twoBackends(), "hash-source-ip")
	if err != nil {
		t.Fatal(err)
	}
	if findNumgen(got) >= 0 {
		t.Fatal("hash pipeline must not contain a numgen expression")
	}
	var hash *expr.Hash
	for _, e := range got {
		if h, ok := e.(*expr.Hash); ok {
			hash = h
		}
	}
	if hash == nil {
		t.Fatal("hash pipeline has no hash expression")
	}
	if hash.Modulus != 2 || hash.Length != 4 || hash.Type != expr.HashTypeJenkins {
		t.Fatalf("unexpected hash: %+v", hash)
	}
	// The hash reads the source address: an saddr payload must precede it.
	hashAt := -1
	saddrAt := -1
	for i, e := range got {
		if p, ok := e.(*expr.Payload); ok && p.Offset == 0x0c && p.Len == 0x4 {
			saddrAt = i
		}
		if _, ok := e.(*expr.Hash); ok {
			hashAt = i
		}
	}
	if saddrAt < 0 || hashAt < 0 || saddrAt > hashAt {
		t.Fatalf("saddr payload (at %d) must precede the hash (at %d)", saddrAt, hashAt)
	}
	if findLookup(got) == nil {
		t.Fatal("hash pipeline lost its map lookup")
	}
	if !hasDNAT(got) {
		t.Fatal("hash pipeline lost its DNAT")
	}
}

func TestBuildLBExprs_UnknownPolicy(t *testing.T) {
	c := newTestConn(t)
	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
	if _, err := buildLBExprs(c, "sn", table, net.ParseIP("10.44.0.50"), "tcp", 80, twoBackends(), "least-conn"); err == nil {
		t.Fatal("expected an error for least-conn")
	}
}

func TestPlanRules_LBPolicyDispatch(t *testing.T) {
	for _, tc := range []struct {
		policy  string
		wantErr bool
	}{
		{"", false},
		{"round-robin", false},
		{"random", false},
		{"hash-source-ip", false},
		{"least-conn", true},
	} {
		t.Run("policy="+tc.policy, func(t *testing.T) {
			m := &Manager{conn: newTestConn(t), logger: testLogger()}
			ss := newFakeSetStore()
			table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4}
			chain := &nftables.Chain{Name: "pre_x", Table: table}
			svc := deniedService()
			svc.DeniedCIDRs = nil
			svc.LBPolicy = tc.policy
			_, err := m.planRules(m.conn, ss, table, chain, svc)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
