package geoip

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vranyes/homelab-ingress/ingress/internal/config"
)

const (
	maxmindDownloadBase = "https://download.maxmind.com/geoip/databases"
	dbipDownloadBase    = "https://download.db-ip.com/free"
)

func buildMaxMindDownloadURL(edition, licenseKey string) string {
	return fmt.Sprintf("%s/%s/update?edition_id=%s&license_key=%s",
		maxmindDownloadBase,
		url.PathEscape(edition),
		url.QueryEscape(edition),
		url.QueryEscape(licenseKey),
	)
}

// buildDBIPDownloadURL returns the current month's DB-IP IP-to-Country Lite
// MMDB. DB-IP publishes one file per calendar month and requires no key or
// account. The file is licensed CC BY 4.0; see https://db-ip.com.
func buildDBIPDownloadURL(now time.Time) string {
	return fmt.Sprintf("%s/dbip-country-lite-%s.mmdb.gz",
		dbipDownloadBase, now.UTC().Format("2006-01"))
}

// databaseStale reports whether the cached database needs a refresh. The second
// return value says whether the file exists at all.
func databaseStale(path string, staleAfter time.Duration, now time.Time) (stale bool, exists bool) {
	info, err := os.Stat(path)
	if err != nil {
		return true, false
	}
	if staleAfter <= 0 {
		return false, true
	}
	return now.Sub(info.ModTime()) > staleAfter, true
}

func (m *Manager) downloadURL() string {
	if m.url != "" {
		return m.url
	}
	if m.cfg.DownloadURL != "" {
		return m.cfg.DownloadURL
	}
	if m.cfg.Provider == config.ProviderDBIP {
		return buildDBIPDownloadURL(time.Now())
	}
	return buildMaxMindDownloadURL(m.cfg.Edition, m.cfg.LicenseKey)
}

// download fetches the provider's archive and atomically replaces the cache. A
// failed download leaves the previous file untouched.
func (m *Manager) download(ctx context.Context) error {
	if m.httpClient == nil {
		m.httpClient = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.downloadURL(), nil)
	if err != nil {
		return err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", m.cfg.Provider, resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()

	dir := filepath.Dir(m.cfg.CachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".geoip-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if m.cfg.Provider == config.ProviderDBIP {
		// DB-IP ships a gzip of the raw MMDB, not a tarball.
		if _, err := io.Copy(tmp, gz); err != nil {
			return fmt.Errorf("extract DB-IP database: %w", err)
		}
	} else if err := extractTarMMDB(tmp, gz); err != nil {
		return err
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := looksLikeMMDB(tmpName); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, m.cfg.CachePath); err != nil {
		return fmt.Errorf("replace cache: %w", err)
	}
	return nil
}

// extractTarMMDB copies the first regular .mmdb member of a MaxMind tarball.
func extractTarMMDB(dst io.Writer, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tarball: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || !strings.HasSuffix(hdr.Name, ".mmdb") {
			continue
		}
		if _, err := io.Copy(dst, tr); err != nil {
			return fmt.Errorf("extract %s: %w", hdr.Name, err)
		}
		return nil
	}
	return fmt.Errorf("tarball contains no .mmdb")
}
