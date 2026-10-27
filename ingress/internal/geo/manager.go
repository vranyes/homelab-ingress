// Package geo keeps an in-memory country→CIDR cache in sync with the MaxMind
// database file and materialises the per-service CIDR union that the nftables
// layer installs.
package geo

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

// DefaultPollInterval is the fallback cadence for bind mounts where inotify
// events may never fire.
const DefaultPollInterval = 30 * time.Second

// Source is the reloadable MaxMind database surface.
type Source interface {
	Reload() error
	CountryCIDRs(cc string) ([]*net.IPNet, error)
	LookupCountry(net.IP) (string, bool)
}

// RebuildFunc receives the concatenated CIDRs for every service that has a
// non-empty geo allow-list.
type RebuildFunc func(map[string][]*net.IPNet)

type Manager struct {
	src    Source
	st     *state.State
	logger *slog.Logger
	path   string

	pollInterval time.Duration
	disableWatch bool // tests only

	mu        sync.RWMutex
	onRebuild RebuildFunc

	// Stat of the file as of the last reload attempt, guarded by mu.
	lastMod  time.Time
	lastSize int64
}

// Option customises a Manager.
type Option func(*Manager)

// WithPollInterval overrides the fallback poller cadence.
func WithPollInterval(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.pollInterval = d
		}
	}
}

func NewManager(src Source, st *state.State, logger *slog.Logger, path string, opts ...Option) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		src:          src,
		st:           st,
		logger:       logger,
		path:         path,
		pollInterval: DefaultPollInterval,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// OnRebuild registers the callback fired after every successful reload.
func (m *Manager) OnRebuild(fn RebuildFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRebuild = fn
}

// CountryCIDRs returns the cached CIDRs for a country code. The bool is true
// only when at least one network is known.
func (m *Manager) CountryCIDRs(cc string) ([]*net.IPNet, bool) {
	nets, err := m.src.CountryCIDRs(strings.ToUpper(strings.TrimSpace(cc)))
	if err != nil {
		m.logger.Error("geoip country lookup failed", "country", cc, "error", err)
		return nil, false
	}
	return nets, len(nets) > 0
}

// LookupCountry resolves an IP to its ISO-3166 alpha-2 code.
func (m *Manager) LookupCountry(ip net.IP) (string, bool) {
	return m.src.LookupCountry(ip)
}

// SnapshotInUse returns the country codes referenced by at least one service.
func (m *Manager) SnapshotInUse() map[string]struct{} {
	out := make(map[string]struct{})
	if m.st == nil {
		return out
	}
	for _, svc := range m.st.Clone().Services {
		if svc == nil {
			continue
		}
		for _, cc := range svc.AllowedGeos {
			out[cc] = struct{}{}
		}
	}
	return out
}

// ServiceCIDRs builds the per-service union map consumed by
// nftables.Manager.RefreshGeoSets (see ADR 0004). A service is included when
// it has a non-empty country allow-list OR a non-empty static CIDR
// allow-list; the union covers both. Static CIDRs are parsed defensively —
// the RPC layer already validated them, but state may predate validation —
// and invalid entries are skipped with a log.
func (m *Manager) ServiceCIDRs() map[string][]*net.IPNet {
	out := make(map[string][]*net.IPNet)
	if m.st == nil {
		return out
	}
	for key, svc := range m.st.Clone().Services {
		if svc == nil || (len(svc.AllowedGeos) == 0 && len(svc.AllowedCIDRs) == 0) {
			continue
		}
		var union []*net.IPNet
		for _, cc := range svc.AllowedGeos {
			nets, err := m.src.CountryCIDRs(cc)
			if err != nil {
				m.logger.Error("geoip country lookup failed", "service", key, "country", cc, "error", err)
				continue
			}
			union = append(union, nets...)
		}
		for _, raw := range svc.AllowedCIDRs {
			_, ipNet, err := net.ParseCIDR(raw)
			if err != nil {
				m.logger.Error("skipping invalid static CIDR", "service", key, "cidr", raw, "error", err)
				continue
			}
			if ipNet.IP.To4() == nil {
				m.logger.Error("skipping non-IPv4 static CIDR", "service", key, "cidr", raw)
				continue
			}
			union = append(union, ipNet)
		}
		out[key] = union
	}
	return out
}

// Reload re-reads the database and fires the rebuild callback.
func (m *Manager) Reload() error {
	// Record the metadata before reading. If a writer lands between the stat
	// and the read, the next poll sees a newer mtime and reloads again;
	// recording after the read could record the newer file while holding the
	// older contents and miss the change entirely.
	m.recordStat()
	if err := m.src.Reload(); err != nil {
		return err
	}

	m.mu.RLock()
	fn := m.onRebuild
	m.mu.RUnlock()
	if fn != nil {
		fn(m.ServiceCIDRs())
	}
	return nil
}

// Run loads the database, then watches the file and polls as a fallback until
// ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	watcher, err := m.newWatcher()
	if err != nil {
		m.logger.Warn("geo file watcher unavailable, relying on poller", "error", err)
	}

	if err := m.Reload(); err != nil {
		m.logger.Error("initial geo reload failed", "error", err)
	}

	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	var events <-chan fsnotify.Event
	var errs <-chan error
	if watcher != nil {
		events = watcher.Events
		errs = watcher.Errors
	}

	for {
		select {
		case <-ctx.Done():
			if watcher != nil {
				_ = watcher.Close()
			}
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				errs = nil
				continue
			}
			if m.relevantEvent(ev) {
				_ = m.reloadAndLog("file event")
			}
		case err := <-errs:
			m.logger.Warn("geo file watcher error", "error", err)
		case <-ticker.C:
			if m.fileChanged() {
				_ = m.reloadAndLog("poller")
			}
		}
	}
}

func (m *Manager) reloadAndLog(reason string) error {
	if err := m.Reload(); err != nil {
		m.logger.Error("geo reload failed", "reason", reason, "error", err)
		return err
	}
	m.logger.Info("geo database reloaded", "reason", reason, "path", m.path)
	return nil
}

func (m *Manager) newWatcher() (*fsnotify.Watcher, error) {
	if m.disableWatch {
		return nil, nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	// Watch the parent directory: a download replaces the file with a rename,
	// which detaches a watch registered on the old inode.
	dir := filepath.Dir(m.path)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}
	return w, nil
}

func (m *Manager) relevantEvent(ev fsnotify.Event) bool {
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
		return false
	}
	return filepath.Clean(ev.Name) == filepath.Clean(m.path)
}

func (m *Manager) fileChanged() bool {
	info, err := os.Stat(m.path)
	if err != nil {
		return false
	}
	m.mu.RLock()
	lastMod, lastSize := m.lastMod, m.lastSize
	m.mu.RUnlock()
	return !info.ModTime().Equal(lastMod) || info.Size() != lastSize
}

func (m *Manager) recordStat() {
	info, err := os.Stat(m.path)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.lastMod = info.ModTime()
	m.lastSize = info.Size()
	m.mu.Unlock()
}
