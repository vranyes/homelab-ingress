package geoip

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vranyes/homelab-ingress/ingress/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

const fixtureTSV = `10.44.0.0/26	US
10.44.0.64/26	CA
10.44.0.128/26	EU
10.44.0.192/26	XX
`

func TestTSVReader_LookupCountry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mock-geo.tsv")
	writeFixture(t, path, fixtureTSV)

	r, err := openTSV(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	tests := []struct {
		ip   string
		want string
		ok   bool
	}{
		{"10.44.0.1", "US", true},   // bridge address
		{"10.44.0.2", "US", true},   // ns-server
		{"10.44.0.65", "CA", true},  // reserved
		{"10.44.0.129", "EU", true}, // ns-server secondary
		{"10.44.0.200", "XX", true}, // reserved
		{"192.0.2.1", "", false},
	}
	for _, tc := range tests {
		got, ok := r.LookupCountry(net.ParseIP(tc.ip))
		if got != tc.want || ok != tc.ok {
			t.Errorf("LookupCountry(%s) = (%q, %v), want (%q, %v)", tc.ip, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTSVReader_CountryCIDRs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mock-geo.tsv")
	writeFixture(t, path, fixtureTSV)
	r, err := openTSV(path)
	if err != nil {
		t.Fatal(err)
	}

	nets, err := r.CountryCIDRs("us")
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 1 || nets[0].String() != "10.44.0.0/26" {
		t.Fatalf("US CIDRs = %v", nets)
	}
	// Callers may mutate the returned slice without corrupting the cache.
	nets[0] = nil
	again, _ := r.CountryCIDRs("US")
	if again[0] == nil {
		t.Fatal("CountryCIDRs returned a shared slice")
	}

	empty, err := r.CountryCIDRs("ZZ")
	if err != nil || len(empty) != 0 {
		t.Fatalf("unknown country = %v, %v", empty, err)
	}
}

func TestTSVReader_RejectsMalformedFixture(t *testing.T) {
	tests := map[string]string{
		"bad cidr":   "10.44.0.0/33 US\n",
		"bad code":   "10.44.0.0/26 usa\n",
		"missing cc": "10.44.0.0/26\n",
		"ipv6":       "fd00::/64 US\n",
		"empty":      "# only a comment\n",
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mock-geo.tsv")
			writeFixture(t, path, contents)
			if _, err := openTSV(path); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestManager_MockReloadSwapsReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mock-geo.tsv")
	writeFixture(t, path, fixtureTSV)

	m, err := New(config.GeoIP{Provider: config.ProviderMock, CachePath: path}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if cc, ok := m.LookupCountry(net.ParseIP("10.44.0.2")); !ok || cc != "US" {
		t.Fatalf("before reload: (%q, %v)", cc, ok)
	}

	// Swap the fixture: the bridge range now maps to EU.
	writeFixture(t, path, "10.44.0.0/26\tEU\n")
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if cc, ok := m.LookupCountry(net.ParseIP("10.44.0.2")); !ok || cc != "EU" {
		t.Fatalf("after reload: (%q, %v)", cc, ok)
	}
	nets, err := m.CountryCIDRs("EU")
	if err != nil || len(nets) != 1 || nets[0].String() != "10.44.0.0/26" {
		t.Fatalf("EU CIDRs = %v, %v", nets, err)
	}
}

func TestManager_ReloadKeepsOldReaderOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mock-geo.tsv")
	writeFixture(t, path, fixtureTSV)

	m, err := New(config.GeoIP{Provider: config.ProviderMock, CachePath: path}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err == nil {
		t.Fatal("expected reload to fail")
	}
	if cc, ok := m.LookupCountry(net.ParseIP("10.44.0.2")); !ok || cc != "US" {
		t.Fatalf("previous reader was discarded: (%q, %v)", cc, ok)
	}
}

func TestManager_RunMockExitsOnCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mock-geo.tsv")
	writeFixture(t, path, fixtureTSV)
	m, err := New(config.GeoIP{Provider: config.ProviderMock, CachePath: path}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestDatabaseStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geo.mmdb")
	writeFixture(t, path, "x")

	stale, exists := databaseStale(path, 24*time.Hour, time.Now())
	if !exists || stale {
		t.Fatalf("fresh file: stale=%v exists=%v", stale, exists)
	}

	stale, exists = databaseStale(path, time.Millisecond, time.Now().Add(time.Hour))
	if !exists || !stale {
		t.Fatalf("old file: stale=%v exists=%v", stale, exists)
	}

	stale, exists = databaseStale(filepath.Join(t.TempDir(), "nope"), time.Hour, time.Now())
	if exists || !stale {
		t.Fatalf("missing file: stale=%v exists=%v", stale, exists)
	}
}

// tarball builds a gzipped tar containing one file at name.
func tarball(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestDownloadManager(t *testing.T, cachePath, serverURL string) *Manager {
	t.Helper()
	return &Manager{
		cfg: config.GeoIP{
			Provider:   config.ProviderMaxMind,
			LicenseKey: "test-key",
			Edition:    config.DefaultGeoIPEdition,
			CachePath:  cachePath,
		},
		logger:     discardLogger(),
		url:        serverURL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func TestDownload_AtomicReplace(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "geolite2-country.mmdb")
	writeFixture(t, cache, "old")

	want := []byte("new database bytes" + mmdbMarker)
	payload := tarball(t, "GeoLite2-Country_20260801/GeoLite2-Country.mmdb", want)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := newTestDownloadManager(t, cache, srv.URL)
	if err := m.download(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("cache = %q, want %q", got, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestDownload_FailureKeepsPreviousCache(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "geolite2-country.mmdb")
	writeFixture(t, cache, "old")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	m := newTestDownloadManager(t, cache, srv.URL)
	if err := m.download(context.Background()); err == nil {
		t.Fatal("expected download to fail")
	}
	got, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("cache was clobbered: %q", got)
	}
}

func TestDownload_RejectsTarballWithoutMMDB(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "geo.mmdb")
	payload := tarball(t, "README.txt", []byte("nope"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := newTestDownloadManager(t, cache, srv.URL)
	if err := m.download(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestOpenMMDB_RejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geo.mmdb")
	writeFixture(t, path, "not an mmdb")
	if _, err := openMMDB(path); err == nil {
		t.Fatal("expected an error")
	}
}

func TestBuildDBIPDownloadURL(t *testing.T) {
	// 2026-11-01T00:30:00+02:00 is still October in UTC; the URL must use UTC.
	now := time.Date(2026, 11, 1, 0, 30, 0, 0, time.FixedZone("CEST", 2*60*60))
	got := buildDBIPDownloadURL(now)
	want := "https://download.db-ip.com/free/dbip-country-lite-2026-10.mmdb.gz"
	if got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func gzipped(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newDBIPDownloadManager(t *testing.T, cachePath, serverURL string) *Manager {
	t.Helper()
	return &Manager{
		cfg: config.GeoIP{
			Provider:  config.ProviderDBIP,
			CachePath: cachePath,
		},
		logger:     discardLogger(),
		url:        serverURL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func TestDownload_DBIPRawGzip(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "dbip-country-lite.mmdb")
	want := []byte("dbip database bytes" + mmdbMarker)
	payload := gzipped(t, want)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := newDBIPDownloadManager(t, cache, srv.URL)
	if err := m.download(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("cache = %q, want %q", got, want)
	}
}

func TestDownload_RejectsNonMMDB(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "dbip-country-lite.mmdb")
	writeFixture(t, cache, "old")

	payload := gzipped(t, []byte("<html>rate limited</html>"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := newDBIPDownloadManager(t, cache, srv.URL)
	if err := m.download(context.Background()); err == nil {
		t.Fatal("expected the download to be rejected")
	}
	got, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("cache was clobbered: %q", got)
	}
}

func TestRefreshDue_DBIP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dbip-country-lite.mmdb")
	writeFixture(t, path, "x")
	cached := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, cached, cached); err != nil {
		t.Fatal(err)
	}

	m := &Manager{cfg: config.GeoIP{Provider: config.ProviderDBIP, CachePath: path}}
	if m.refreshDue(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("same-month cache must not refresh")
	}
	if !m.refreshDue(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("new-month cache must refresh")
	}

	m.cfg.StaleAfter = 7 * 24 * time.Hour
	if !m.refreshDue(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("a cache older than stale_after must refresh")
	}
	m.cfg.StaleAfter = 0

	m.cfg.DownloadURL = "https://mirror.example/db.mmdb.gz"
	if !m.refreshDue(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("a custom download_url must refresh on every tick")
	}
}

func TestEnsureDatabase_DBIPSkipsCurrentMonth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dbip-country-lite.mmdb")
	writeFixture(t, path, "cached")
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := newDBIPDownloadManager(t, path, srv.URL)
	if err := m.ensureDatabase(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatalf("downloaded a current-month database (%d requests)", hits)
	}
}

func TestEnsureDatabase_DBIPDownloadsWhenMonthChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dbip-country-lite.mmdb")
	writeFixture(t, path, "old")
	lastMonth := time.Now().AddDate(0, -1, 0)
	if err := os.Chtimes(path, lastMonth, lastMonth); err != nil {
		t.Fatal(err)
	}

	want := []byte("fresh database" + mmdbMarker)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(gzipped(t, want))
	}))
	defer srv.Close()

	m := newDBIPDownloadManager(t, path, srv.URL)
	if err := m.ensureDatabase(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("cache = %q, want %q", got, want)
	}
}
