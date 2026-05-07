package config

import (
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RouterAddr != DefaultRouterAddr {
		t.Errorf("expected router_addr %q, got %q", DefaultRouterAddr, cfg.RouterAddr)
	}
	if cfg.PollInterval != DefaultPollInterval {
		t.Errorf("expected poll_interval %v, got %v", DefaultPollInterval, cfg.PollInterval)
	}
	if cfg.MetricsAddr != DefaultMetricsAddr {
		t.Errorf("expected metrics_addr %q, got %q", DefaultMetricsAddr, cfg.MetricsAddr)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "valid config",
			cfg:     Config{RouterAddr: "localhost:50051", PollInterval: 30 * time.Second},
			wantErr: false,
		},
		{
			name:    "missing router addr",
			cfg:     Config{RouterAddr: "", PollInterval: 30 * time.Second},
			wantErr: true,
		},
		{
			name:    "zero poll interval",
			cfg:     Config{RouterAddr: "localhost:50051", PollInterval: 0},
			wantErr: true,
		},
		{
			name:    "negative poll interval",
			cfg:     Config{RouterAddr: "localhost:50051", PollInterval: -10 * time.Second},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("expected error=%v, got %v", tt.wantErr, err)
			}
		})
	}
}
