package geo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/vranyes/homelab-ingress/ingress/internal/metrics"
)

var errFake = errors.New("fake reload failure")

// TestReloadSetsDBAgeAndReconcileMetric pins the observability hook in
// Reload: a successful reload sets a non-negative db-age gauge and a
// success reconcile counter; a failed reload records an error outcome.
func TestReloadSetsDBAgeAndReconcileMetric(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "geo.mmdb")
	if err := os.WriteFile(dbPath, []byte("fake-db"), 0644); err != nil {
		t.Fatal(err)
	}
	// Make the mtime deterministic-ish: backdate so age is positive.
	past := time.Now().Add(-90 * time.Minute)
	if err := os.Chtimes(dbPath, past, past); err != nil {
		t.Fatal(err)
	}

	st := testState(t, nil)
	src := newFakeSource()
	m := NewManager(src, st, discardLogger(), dbPath, WithPollInterval(time.Hour))

	beforeSuccess := testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues("geo", metrics.ResultSuccess))
	beforeError := testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues("geo", metrics.ResultError))

	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if age := m.dbAgeSeconds(); age < 89*60 || age > 91*60 {
		t.Fatalf("dbAgeSeconds = %v, want ~5400s", age)
	}
	if got := testutil.ToFloat64(metrics.GeoDBAgeSeconds); got < 89*60 || got > 91*60 {
		t.Fatalf("gauge geo_db_age_seconds = %v, want ~5400s", got)
	}
	if got := testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues("geo", metrics.ResultSuccess)); got != beforeSuccess+1 {
		t.Fatalf("reconcile success counter did not advance")
	}

	src.reloadErr = errFake
	if err := m.Reload(); err == nil {
		t.Fatalf("expected reload error")
	}
	if got := testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues("geo", metrics.ResultError)); got != beforeError+1 {
		t.Fatalf("reconcile error counter did not advance")
	}
}

// TestDBAgeSecondsUnknownBeforeReload pins the -1 sentinel: a manager that
// never stat'ed the file reports unknown, and the package default matches.
func TestDBAgeSecondsUnknownBeforeReload(t *testing.T) {
	m := NewManager(newFakeSource(), testState(t, nil), discardLogger(), "/nonexistent/geo.mmdb")
	if got := m.dbAgeSeconds(); got != -1 {
		t.Fatalf("dbAgeSeconds = %v, want -1 before any reload", got)
	}
}
