package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultGRPCListen = "0.0.0.0:50051"
	DefaultWSListen   = "0.0.0.0:50052"
	// DefaultMetricsListen is the Prometheus exposition address. An empty
	// listen address disables the endpoint.
	DefaultMetricsListen = ":9102"

	// ProviderMaxMind downloads GeoLite2 Country from MaxMind (requires a
	// license key).
	ProviderMaxMind = "maxmind"
	// ProviderDBIP downloads the free DB-IP IP-to-Country Lite MMDB. No
	// account or license key is required; the data is CC BY 4.0 and requires
	// attribution to DB-IP (https://db-ip.com).
	ProviderDBIP = "dbip"
	// ProviderMock reads a local TSV fixture (e2e only).
	ProviderMock = "mock"

	DefaultGeoIPEdition         = "GeoLite2-Country"
	DefaultGeoIPRefreshInterval = 24 * time.Hour
	DefaultGeoLite2StaleAfter   = 7 * 24 * time.Hour
	DefaultDBIPStaleAfter       = 35 * 24 * time.Hour
	DefaultGeoLite2CacheFile    = "geolite2-country.mmdb"
	DefaultDBIPCacheFile        = "dbip-country-lite.mmdb"
	DefaultGeoIPMockFile        = "mock-geo.tsv"
)

type Config struct {
	BaseInterface string           `yaml:"base_interface"`
	GRPC          GRPC             `yaml:"grpc"`
	WS            WS               `yaml:"ws"`
	Metrics       Metrics          `yaml:"metrics"`
	Auth          Auth             `yaml:"auth"`
	Masquerade    []MasqueradeRule `yaml:"masquerade"`
	GeoIP         GeoIP            `yaml:"geoip"`
	Healthcheck   Healthcheck      `yaml:"healthcheck"`
}

// Healthcheck tunes active TCP probing of load-balancer backends (see
// internal/health). The whole block is optional: unset fields select the
// health package defaults (10s interval, 2s timeout, 2/2 thresholds).
type Healthcheck struct {
	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	FailThreshold    int           `yaml:"fail_threshold"`
	SuccessThreshold int           `yaml:"success_threshold"`
}

// ApplyDefaults fills unset (non-positive) fields with the health defaults.
func (h *Healthcheck) ApplyDefaults() {
	if h.Interval <= 0 {
		h.Interval = 10 * time.Second
	}
	if h.Timeout <= 0 {
		h.Timeout = 2 * time.Second
	}
	if h.FailThreshold <= 0 {
		h.FailThreshold = 2
	}
	if h.SuccessThreshold <= 0 {
		h.SuccessThreshold = 2
	}
}

// GeoIP holds the country-database settings. The whole block is optional: an
// empty Provider disables geo-blocking (see ADR 0003 and ADR 0006).
type GeoIP struct {
	// Provider selects the database backend: "maxmind", "dbip" or "mock".
	Provider string `yaml:"provider"`
	// LicenseKey is required only for the maxmind provider.
	LicenseKey string `yaml:"license_key,omitempty"`
	// Edition is the MaxMind edition; ignored by other providers.
	Edition string `yaml:"edition,omitempty"`
	// DownloadURL overrides the provider's URL (mirrors/offline use).
	DownloadURL string `yaml:"download_url,omitempty"`

	RefreshInterval time.Duration `yaml:"refresh_interval"`
	CachePath       string        `yaml:"cache_path"`
	StaleAfter      time.Duration `yaml:"stale_after"`
}

// Enabled reports whether geo-blocking is configured.
func (g GeoIP) Enabled() bool { return g.Provider != "" }

// Mock reports whether the TSV-backed e2e reader should be used.
func (g GeoIP) Mock() bool { return g.Provider == ProviderMock }

// ApplyDefaults normalises the provider and fills unset fields. stateDir is
// used to derive the default cache path; an empty stateDir leaves CachePath
// untouched.
func (g *GeoIP) ApplyDefaults(stateDir string) {
	g.Provider = strings.ToLower(strings.TrimSpace(g.Provider))
	// Backwards-compatible inference: a license key with no explicit provider
	// means MaxMind.
	if g.Provider == "" && g.LicenseKey != "" {
		g.Provider = ProviderMaxMind
	}
	// The e2e shortcut predates the provider field.
	if g.LicenseKey == ProviderMock {
		g.Provider = ProviderMock
	}

	if g.Edition == "" {
		g.Edition = DefaultGeoIPEdition
	}
	if g.RefreshInterval <= 0 {
		g.RefreshInterval = DefaultGeoIPRefreshInterval
	}
	if g.StaleAfter <= 0 {
		if g.Provider == ProviderDBIP {
			g.StaleAfter = DefaultDBIPStaleAfter
		} else {
			g.StaleAfter = DefaultGeoLite2StaleAfter
		}
	}
	if g.CachePath == "" && stateDir != "" {
		name := DefaultGeoLite2CacheFile
		switch g.Provider {
		case ProviderDBIP:
			name = DefaultDBIPCacheFile
		case ProviderMock:
			name = DefaultGeoIPMockFile
		}
		g.CachePath = filepath.Join(stateDir, name)
	}
}

// Validate checks provider-specific requirements.
func (g GeoIP) Validate() error {
	switch g.Provider {
	case "":
		return nil
	case ProviderMaxMind:
		if g.LicenseKey == "" {
			return errors.New("geoip.license_key is required for provider maxmind")
		}
		return nil
	case ProviderDBIP, ProviderMock:
		return nil
	default:
		return fmt.Errorf("geoip.provider %q is not supported (want maxmind, dbip or mock)", g.Provider)
	}
}

type GRPC struct {
	Listen string `yaml:"listen"`
}

type WS struct {
	Listen string `yaml:"listen"`
}

// Metrics holds the Prometheus exposition endpoint settings. An empty
// Listen disables the endpoint; this is the zero value so a config that
// omits the block keeps working without serving metrics.
type Metrics struct {
	Listen string `yaml:"listen"`
}

type MasqueradeRule struct {
	InputDevice  string `yaml:"input_device"`
	OutputDevice string `yaml:"output_device"`
}

type Auth struct {
	// Issuer is the OIDC issuer URL (e.g. https://accounts.example.com)
	Issuer string `yaml:"issuer"`
	// Audience is the expected audience (aud claim) of incoming tokens.
	// When set, tokens must include this value in their aud claim.
	Audience string `yaml:"audience"`
	// AllowedSubjects is the list of subject (sub) claim values that are allowed to use the service
	AllowedSubjects []string `yaml:"allowed_subjects"`
	// TrustedRoot is an optional path to a CA certificate file used to verify
	// the TLS connection to the OIDC issuer. When empty, the system CA pool is used.
	TrustedRoot string `yaml:"trusted_root_ca"`
}

func DefaultConfig() Config {
	return Config{
		GRPC: GRPC{
			Listen: DefaultGRPCListen,
		},
		WS: WS{
			Listen: DefaultWSListen,
		},
		Metrics: Metrics{
			Listen: DefaultMetricsListen,
		},
	}
}

func LoadConfig(path string) (Config, error) {
	var cfg Config

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.GRPC.Listen == "" {
		cfg.GRPC.Listen = DefaultGRPCListen
	}
	if cfg.WS.Listen == "" {
		cfg.WS.Listen = DefaultWSListen
	}
	if cfg.Metrics.Listen == "" {
		cfg.Metrics.Listen = DefaultMetricsListen
	}
	cfg.GeoIP.ApplyDefaults("")
	cfg.Healthcheck.ApplyDefaults()
}

func validate(cfg *Config) error {
	if cfg.BaseInterface == "" {
		return errors.New("base_interface is required")
	}
	if cfg.Auth.Issuer == "" {
		return errors.New("auth.issuer is required")
	}
	if len(cfg.Auth.AllowedSubjects) == 0 {
		return errors.New("auth.allowed_subjects is required")
	}
	if cfg.Auth.Audience == "" {
		return errors.New("auth.audience is required")
	}
	if err := cfg.GeoIP.Validate(); err != nil {
		return err
	}
	return nil
}
