package config

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	DefaultGRPCListen = "0.0.0.0:50051"
	DefaultWSListen   = "0.0.0.0:50052"
)

type Config struct {
	BaseInterface string           `yaml:"base_interface"`
	GRPC          GRPC             `yaml:"grpc"`
	WS            WS               `yaml:"ws"`
	Auth          Auth             `yaml:"auth"`
	Masquerade    []MasqueradeRule `yaml:"masquerade"`
}

type GRPC struct {
	Listen string `yaml:"listen"`
}

type WS struct {
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
	return nil
}
