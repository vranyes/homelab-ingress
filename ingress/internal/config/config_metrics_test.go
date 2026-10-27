package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeMetricsConfig(t *testing.T, metricsBlock string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := `base_interface: eth0
auth:
  issuer: https://issuer.example.com
  audience: https://api.example.com
  allowed_subjects:
  - test-subject
` + metricsBlock
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func TestLoadConfig_Metrics(t *testing.T) {
	t.Run("defaults to :9102 when omitted", func(t *testing.T) {
		cfg, err := LoadConfig(writeMetricsConfig(t, ""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Metrics.Listen != DefaultMetricsListen {
			t.Errorf("metrics.listen = %q, want %q", cfg.Metrics.Listen, DefaultMetricsListen)
		}
	})

	t.Run("explicit listen wins", func(t *testing.T) {
		cfg, err := LoadConfig(writeMetricsConfig(t, "metrics:\n  listen: 127.0.0.1:9102\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Metrics.Listen != "127.0.0.1:9102" {
			t.Errorf("metrics.listen = %q", cfg.Metrics.Listen)
		}
	})

	t.Run("DefaultConfig carries the default", func(t *testing.T) {
		if DefaultConfig().Metrics.Listen != DefaultMetricsListen {
			t.Errorf("DefaultConfig metrics.listen = %q", DefaultConfig().Metrics.Listen)
		}
	})
}
