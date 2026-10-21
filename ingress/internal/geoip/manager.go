// Package geoip downloads, opens and caches country-level MMDB databases.
// It supports MaxMind GeoLite2 (license key required) and the free DB-IP
// IP-to-Country Lite database (no key; CC BY 4.0, attribution to DB-IP).
// The e2e suite swaps in a TSV-backed mock reader by setting
// geoip.provider to "mock".
package geoip

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/vranyes/homelab-ingress/ingress/internal/config"
)

// Reader is the country lookup surface consumed by the geo cache.
type Reader interface {
	LookupCountry(net.IP) (string, bool)
	CountryCIDRs(cc string) ([]*net.IPNet, error)
	Close() error
}

// Manager owns the current Reader and swaps it atomically on reload.
type Manager struct {
	cfg    config.GeoIP
	logger *slog.Logger

	mu     sync.RWMutex
	reader Reader

	// Overridable in tests (same package).
	url        string
	httpClient *http.Client
}

// New opens (and, for the real backends, possibly downloads) the configured
// database. In mock mode the cache path is a TSV fixture and no network access
// happens at all.
func New(cfg config.GeoIP, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if !cfg.Enabled() {
		return nil, fmt.Errorf("geoip: no provider configured")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m := &Manager{cfg: cfg, logger: logger}

	if cfg.Mock() {
		r, err := openTSV(cfg.CachePath)
		if err != nil {
			return nil, fmt.Errorf("geoip mock reader: %w", err)
		}
		m.reader = r
		return m, nil
	}

	if err := m.ensureDatabase(context.Background(), time.Now()); err != nil {
		return nil, err
	}
	r, err := openMMDB(cfg.CachePath)
	if err != nil {
		return nil, fmt.Errorf("geoip db: %w", err)
	}
	m.reader = r
	return m, nil
}

// ensureDatabase downloads the database when the cache is missing or due for
// a refresh. A failed download with an existing cache is tolerated so a
// transient provider outage does not take geo-blocking down.
func (m *Manager) ensureDatabase(ctx context.Context, now time.Time) error {
	_, statErr := os.Stat(m.cfg.CachePath)
	exists := statErr == nil
	if m.cfg.Provider == config.ProviderDBIP {
		if !m.refreshDue(now) {
			return nil
		}
	} else {
		stale, _ := databaseStale(m.cfg.CachePath, m.cfg.StaleAfter, now)
		if exists && !stale {
			return nil
		}
	}
	if err := m.download(ctx); err != nil {
		if exists {
			m.logger.Warn("geoip download failed, using stale cache",
				"provider", m.cfg.Provider, "error", err, "path", m.cfg.CachePath)
			return nil
		}
		return fmt.Errorf("download %s database: %w", m.cfg.Provider, err)
	}
	return nil
}

// refreshDue reports whether the DB-IP cache should be replaced. DB-IP Lite
// publishes one file per calendar month, so a cache written this month is
// already current. A custom download_url is refreshed on every tick because
// the operator controls its cadence.
func (m *Manager) refreshDue(now time.Time) bool {
	if m.cfg.DownloadURL != "" {
		return true
	}
	mod := cacheModTime(m.cfg.CachePath)
	if mod.IsZero() {
		return true
	}
	if m.cfg.StaleAfter > 0 && now.Sub(mod) > m.cfg.StaleAfter {
		return true
	}
	return !sameMonth(mod, now)
}

// Run refreshes the database on a timer. In mock mode it only waits for
// cancellation: the TSV fixture is re-read by geo.Manager's file watcher.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if m.cfg.Mock() {
		<-ctx.Done()
		return
	}
	if interval <= 0 {
		interval = config.DefaultGeoIPRefreshInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			if m.cfg.Provider == config.ProviderDBIP && !m.refreshDue(now) {
				continue
			}
			if err := m.download(ctx); err != nil {
				m.logger.Error("geoip refresh failed", "provider", m.cfg.Provider, "error", err)
				continue
			}
			if err := m.Reload(); err != nil {
				m.logger.Error("geoip reload after refresh failed", "error", err)
			}
		}
	}
}

// Reload reopens the database file. The previous reader stays usable if the
// new one cannot be opened.
func (m *Manager) Reload() error {
	var (
		r   Reader
		err error
	)
	if m.cfg.Mock() {
		r, err = openTSV(m.cfg.CachePath)
	} else {
		r, err = openMMDB(m.cfg.CachePath)
	}
	if err != nil {
		return err
	}

	m.mu.Lock()
	old := m.reader
	m.reader = r
	m.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	return nil
}

func (m *Manager) current() Reader {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reader
}

func (m *Manager) LookupCountry(ip net.IP) (string, bool) {
	r := m.current()
	if r == nil {
		return "", false
	}
	return r.LookupCountry(ip)
}

func (m *Manager) CountryCIDRs(cc string) ([]*net.IPNet, error) {
	r := m.current()
	if r == nil {
		return nil, fmt.Errorf("geoip: reader not initialized")
	}
	return r.CountryCIDRs(cc)
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reader == nil {
		return nil
	}
	err := m.reader.Close()
	m.reader = nil
	return err
}

// sameMonth reports whether a and b fall in the same calendar month (UTC).
func sameMonth(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	ay, am, _ := a.UTC().Date()
	by, bm, _ := b.UTC().Date()
	return ay == by && am == bm
}

func cacheModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// mmdbMarker terminates an MMDB metadata section. Checking for it catches HTML
// error pages and truncated downloads before they replace a good cache.
const mmdbMarker = "\xab\xcd\xefMaxMind.com"

func looksLikeMMDB(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	const tail = 128 << 10
	size := info.Size()
	start := int64(0)
	if size > tail {
		start = size - tail
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(bufio.NewReader(f))
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(mmdbMarker)) {
		return errors.New("downloaded file is not an MMDB database")
	}
	return nil
}
