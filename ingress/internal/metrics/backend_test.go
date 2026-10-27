package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBackendHelpers(t *testing.T) {
	SetBackendUp("ns/svc", "10.0.0.1", 8080, false)
	if got := testutil.ToFloat64(BackendUp.WithLabelValues("ns/svc", "10.0.0.1", "8080")); got != 0 {
		t.Fatalf("backend_up = %v, want 0", got)
	}
	SetBackendUp("ns/svc", "10.0.0.1", 8080, true)
	if got := testutil.ToFloat64(BackendUp.WithLabelValues("ns/svc", "10.0.0.1", "8080")); got != 1 {
		t.Fatalf("backend_up = %v, want 1", got)
	}

	IncBackendProbe("ns/svc", true)
	IncBackendProbe("ns/svc", false)
	if got := testutil.ToFloat64(BackendProbesTotal.WithLabelValues("ns/svc", ProbeResultSuccess)); got < 1 {
		t.Fatalf("probes_total{success} = %v, want >= 1", got)
	}
	if got := testutil.ToFloat64(BackendProbesTotal.WithLabelValues("ns/svc", ProbeResultFailure)); got < 1 {
		t.Fatalf("probes_total{failure} = %v, want >= 1", got)
	}

	DeleteBackendUp("ns/svc", "10.0.0.1", 8080)
	if got := testutil.CollectAndCount(BackendUp); got != 0 {
		t.Fatalf("expected backend_up series to be deleted, have %d", got)
	}
}
