package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetricsRegistered(t *testing.T) {
	// Vec families only appear in Gather output once they have at least
	// one child; touch each one so registration itself is what we assert.
	IncNftApplyError(OpAdd)
	SetDHCPLeaseExpiry("test/registration-probe", 1)
	defer DeleteDHCPLeaseExpiry("test/registration-probe")
	ObserveReconcile("test-probe", nil)
	for _, name := range []string{
		"homelab_ingress_services_total",
		"homelab_ingress_nft_apply_errors_total",
		"homelab_ingress_geo_db_age_seconds",
		"homelab_ingress_dhcp_lease_expiry_seconds",
		"homelab_ingress_reconcile_total",
	} {
		found := false
		for _, mf := range mustGather(t) {
			if mf.GetName() == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("metric %q not registered", name)
		}
	}
}

func TestStartEmptyAddrIsNoop(t *testing.T) {
	// Must return immediately without serving or failing.
	Start("", nil)
	Start("", testLogger())
}

func TestHelpers(t *testing.T) {
	SetServicesTotal(3)
	if got := testutil.ToFloat64(ServicesTotal); got != 3 {
		t.Fatalf("services_total = %v, want 3", got)
	}

	IncNftApplyError(OpAdd)
	IncNftApplyError(OpRemove)
	IncNftApplyError(OpRefresh)
	for _, op := range []string{OpAdd, OpRemove, OpRefresh} {
		if got := testutil.ToFloat64(NftApplyErrors.WithLabelValues(op)); got < 1 {
			t.Fatalf("nft_apply_errors_total{op=%q} = %v, want >= 1", op, got)
		}
	}

	SetGeoDBAgeSeconds(42.5)
	if got := testutil.ToFloat64(GeoDBAgeSeconds); got != 42.5 {
		t.Fatalf("geo_db_age_seconds = %v, want 42.5", got)
	}

	SetDHCPLeaseExpiry("ns/svc", 1700000000)
	if got := testutil.ToFloat64(DHCPLeaseExpirySeconds.WithLabelValues("ns/svc")); got != 1700000000 {
		t.Fatalf("dhcp_lease_expiry_seconds = %v", got)
	}
	DeleteDHCPLeaseExpiry("ns/svc")
	if got := testutil.CollectAndCount(DHCPLeaseExpirySeconds); got != 0 {
		t.Fatalf("expected expiry series to be deleted, have %d", got)
	}

	ObserveReconcile("router", nil)
	ObserveReconcile("geo", errTest)
	if got := testutil.ToFloat64(ReconcileTotal.WithLabelValues("router", ResultSuccess)); got < 1 {
		t.Fatalf("reconcile_total{router,success} = %v", got)
	}
	if got := testutil.ToFloat64(ReconcileTotal.WithLabelValues("geo", ResultError)); got < 1 {
		t.Fatalf("reconcile_total{geo,error} = %v", got)
	}
}

func TestMetricsEndpointServes(t *testing.T) {
	Start("127.0.0.1:0", testLogger()) // ephemeral port; only proves no panic
	names := map[string]bool{}
	for _, mf := range mustGather(t) {
		names[mf.GetName()] = true
	}
	for _, name := range []string{
		"homelab_ingress_services_total",
		"homelab_ingress_nft_apply_errors_total",
	} {
		if !names[name] {
			t.Errorf("exposition missing %q", name)
		}
	}
}
