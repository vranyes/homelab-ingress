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
state_file: /var/lib/homelab-lb/allocations.json
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
		if cfg.StateFile != "/var/lib/homelab-lb/allocations.json" {
			t.Errorf("expected state_file /var/lib/homelab-lb/allocations.json, got %q", cfg.StateFile)
		}
		if cfg.GRPC.Listen != "0.0.0.0:50051" {
			t.Errorf("expected grpc.listen 0.0.0.0:50051, got %q", cfg.GRPC.Listen)
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
		if cfg.StateFile != DefaultStateFile {
			t.Errorf("expected default state_file %q, got %q", DefaultStateFile, cfg.StateFile)
		}
	})

	t.Run("returns error when base_interface is empty", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		content := `base_interface: ""
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

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.BaseInterface != "" {
		t.Errorf("expected empty base_interface, got %q", cfg.BaseInterface)
	}
	if cfg.StateFile != DefaultStateFile {
		t.Errorf("expected state_file %q, got %q", DefaultStateFile, cfg.StateFile)
	}
	if cfg.GRPC.Listen != DefaultGRPCListen {
		t.Errorf("expected grpc.listen %q, got %q", DefaultGRPCListen, cfg.GRPC.Listen)
	}
}
