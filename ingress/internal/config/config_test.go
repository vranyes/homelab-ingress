package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Run("loads valid config from file", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		content := `base_interface: wan.201
auth:
  issuer: https://issuer.example.com
  audience: https://api.example.com
  allowed_subjects:
  - test-subject
grpc:
  listen: "0.0.0.0:50051"
`
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if cfg.BaseInterface != "wan.201" {
			t.Errorf("expected base_interface wan.201, got %q", cfg.BaseInterface)
		}
		if cfg.GRPC.Listen != "0.0.0.0:50051" {
			t.Errorf("expected grpc.listen 0.0.0.0:50051, got %q", cfg.GRPC.Listen)
		}
		if cfg.Auth.Issuer != "https://issuer.example.com" {
			t.Errorf("expected issuer, got %q", cfg.Auth.Issuer)
		}
	})

	t.Run("returns error for missing file", func(t *testing.T) {
		_, err := LoadConfig("/nonexistent/config.yaml")
		if err == nil {
			t.Fatal("expected error for missing file, got nil")
		}
	})

	t.Run("returns error for invalid yaml", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(cfgPath, []byte(":::invalid:::"), 0644); err != nil {
			t.Fatal(err)
		}

		_, err := LoadConfig(cfgPath)
		if err == nil {
			t.Fatal("expected error for invalid yaml, got nil")
		}
	})

	t.Run("applies defaults when config is minimal", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		content := `base_interface: eth0
auth:
  issuer: https://issuer.example.com
  audience: https://api.example.com
  allowed_subjects:
  - test-subject
`
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if cfg.GRPC.Listen != DefaultGRPCListen {
			t.Errorf("expected default grpc.listen %q, got %q", DefaultGRPCListen, cfg.GRPC.Listen)
		}
	})

	t.Run("returns error when base_interface is empty", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		content := `base_interface: ""
auth:
  issuer: https://issuer.example.com
  allowed_subjects:
  - test-subject
`
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		_, err := LoadConfig(cfgPath)
		if err == nil {
			t.Fatal("expected error for empty base_interface, got nil")
		}
	})
}

func TestLoadConfig_GeoIPDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `base_interface: eth0
auth:
  issuer: https://issuer.example.com
  audience: https://api.example.com
  allowed_subjects:
  - test-subject
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("a missing geoip block must not be an error: %v", err)
	}
	if cfg.GeoIP.Enabled() {
		t.Fatal("expected geo-blocking to be disabled")
	}
	if cfg.GeoIP.Edition != DefaultGeoIPEdition {
		t.Errorf("edition default = %q, want %q", cfg.GeoIP.Edition, DefaultGeoIPEdition)
	}
	if cfg.GeoIP.RefreshInterval != DefaultGeoIPRefreshInterval {
		t.Errorf("refresh default = %v, want %v", cfg.GeoIP.RefreshInterval, DefaultGeoIPRefreshInterval)
	}
	if cfg.GeoIP.StaleAfter != DefaultGeoLite2StaleAfter {
		t.Errorf("stale_after default = %v, want %v", cfg.GeoIP.StaleAfter, DefaultGeoLite2StaleAfter)
	}
}

func TestLoadConfig_GeoIPMaxMind(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `base_interface: eth0
auth:
  issuer: https://issuer.example.com
  audience: https://api.example.com
  allowed_subjects:
  - test-subject
geoip:
  license_key: secret
  edition: GeoLite2-Country
  refresh_interval: 12h
  cache_path: /var/lib/homelab-ingress/geo.mmdb
  stale_after: 48h
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.GeoIP.Enabled() {
		t.Fatal("expected geo-blocking to be enabled")
	}
	if cfg.GeoIP.Provider != ProviderMaxMind {
		t.Errorf("provider = %q, want maxmind (inferred from license_key)", cfg.GeoIP.Provider)
	}
	if cfg.GeoIP.LicenseKey != "secret" {
		t.Errorf("license key = %q", cfg.GeoIP.LicenseKey)
	}
	if cfg.GeoIP.RefreshInterval.Hours() != 12 {
		t.Errorf("refresh interval = %v", cfg.GeoIP.RefreshInterval)
	}
	if cfg.GeoIP.StaleAfter.Hours() != 48 {
		t.Errorf("stale after = %v", cfg.GeoIP.StaleAfter)
	}
	if cfg.GeoIP.CachePath != "/var/lib/homelab-ingress/geo.mmdb" {
		t.Errorf("cache path = %q", cfg.GeoIP.CachePath)
	}
}

func TestLoadConfig_GeoIPDBIP(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `base_interface: eth0
auth:
  issuer: https://issuer.example.com
  audience: https://api.example.com
  allowed_subjects:
  - test-subject
geoip:
  provider: dbip
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.GeoIP.Enabled() {
		t.Fatal("expected geo-blocking to be enabled without a license key")
	}
	if cfg.GeoIP.Provider != ProviderDBIP {
		t.Errorf("provider = %q", cfg.GeoIP.Provider)
	}
	if cfg.GeoIP.LicenseKey != "" {
		t.Errorf("license key = %q, want empty", cfg.GeoIP.LicenseKey)
	}
	if cfg.GeoIP.StaleAfter != DefaultDBIPStaleAfter {
		t.Errorf("stale_after = %v, want %v", cfg.GeoIP.StaleAfter, DefaultDBIPStaleAfter)
	}
}

func TestLoadConfig_GeoIPValidation(t *testing.T) {
	write := func(t *testing.T, geoBlock string) string {
		t.Helper()
		cfgPath := filepath.Join(t.TempDir(), "config.yaml")
		content := `base_interface: eth0
auth:
  issuer: https://issuer.example.com
  audience: https://api.example.com
  allowed_subjects:
  - test-subject
` + geoBlock
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return cfgPath
	}

	t.Run("unknown provider rejected", func(t *testing.T) {
		if _, err := LoadConfig(write(t, "geoip:\n  provider: ipinfo\n")); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("maxmind requires a license key", func(t *testing.T) {
		if _, err := LoadConfig(write(t, "geoip:\n  provider: maxmind\n")); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("mock does not require a key", func(t *testing.T) {
		if _, err := LoadConfig(write(t, "geoip:\n  provider: mock\n")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestGeoIPApplyDefaults(t *testing.T) {
	t.Run("maxmind cache path defaults under state dir", func(t *testing.T) {
		g := GeoIP{LicenseKey: "secret"}
		g.ApplyDefaults("/var/lib/hl")
		if g.Provider != ProviderMaxMind {
			t.Errorf("provider = %q", g.Provider)
		}
		want := filepath.Join("/var/lib/hl", DefaultGeoLite2CacheFile)
		if g.CachePath != want {
			t.Errorf("cache path = %q, want %q", g.CachePath, want)
		}
	})

	t.Run("dbip cache path and monthly stale window", func(t *testing.T) {
		g := GeoIP{Provider: ProviderDBIP}
		g.ApplyDefaults("/var/lib/hl")
		want := filepath.Join("/var/lib/hl", DefaultDBIPCacheFile)
		if g.CachePath != want {
			t.Errorf("cache path = %q, want %q", g.CachePath, want)
		}
		if g.StaleAfter != DefaultDBIPStaleAfter {
			t.Errorf("stale_after = %v, want %v", g.StaleAfter, DefaultDBIPStaleAfter)
		}
		if g.RefreshInterval != DefaultGeoIPRefreshInterval {
			t.Errorf("refresh = %v", g.RefreshInterval)
		}
	})

	t.Run("mock cache path defaults to the TSV fixture", func(t *testing.T) {
		g := GeoIP{Provider: ProviderMock}
		g.ApplyDefaults("/var/lib/hl")
		want := filepath.Join("/var/lib/hl", DefaultGeoIPMockFile)
		if g.CachePath != want {
			t.Errorf("cache path = %q, want %q", g.CachePath, want)
		}
	})

	t.Run("explicit cache path wins", func(t *testing.T) {
		g := GeoIP{Provider: ProviderMock, CachePath: "/tmp/geo.tsv"}
		g.ApplyDefaults("/var/lib/hl")
		if g.CachePath != "/tmp/geo.tsv" {
			t.Errorf("cache path = %q", g.CachePath)
		}
	})

	t.Run("legacy mock license key selects the mock provider", func(t *testing.T) {
		g := GeoIP{LicenseKey: "mock"}
		g.ApplyDefaults("")
		if g.Provider != ProviderMock {
			t.Errorf("provider = %q", g.Provider)
		}
	})
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.BaseInterface != "" {
		t.Errorf("expected empty base_interface, got %q", cfg.BaseInterface)
	}
	if cfg.GRPC.Listen != DefaultGRPCListen {
		t.Errorf("expected grpc.listen %q, got %q", DefaultGRPCListen, cfg.GRPC.Listen)
	}
}

func TestLoadConfig_Validation(t *testing.T) {
	t.Run("returns error when auth.issuer is missing", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		content := `base_interface: eth0
auth:
  audience: https://api.example.com
  allowed_subjects:
  - test-subject
`
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(cfgPath)
		if err == nil {
			t.Fatal("expected error for missing issuer, got nil")
		}
	})

	t.Run("returns error when auth.allowed_subjects is empty", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		content := `base_interface: eth0
auth:
  issuer: https://issuer.example.com
  audience: https://api.example.com
  allowed_subjects:
`
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(cfgPath)
		if err == nil {
			t.Fatal("expected error for empty allowed_subjects, got nil")
		}
	})
}
