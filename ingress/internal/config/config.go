package config

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	DefaultGRPCListen = "0.0.0.0:50051"
)

type Config struct {
	BaseInterface string `yaml:"base_interface"`
	GRPC          GRPC   `yaml:"grpc"`
	Auth          Auth   `yaml:"auth"`
}

type GRPC struct {
	Listen string `yaml:"listen"`
}

type Auth struct {
	// Issuer is the OIDC issuer URL (e.g. https://accounts.example.com)
	Issuer string `yaml:"issuer"`
	// Audience is the expected audience (aud claim) of incoming tokens.
	// When set, tokens must include this value in their aud claim.
	Audience string `yaml:"audience"`
	// AllowedSubjects is the list of subject (sub) claim values that are allowed to use the service
	AllowedSubjects []string `yaml:"allowed_subjects"`
}

func DefaultConfig() Config {
	return Config{
		GRPC: GRPC{
			Listen: DefaultGRPCListen,
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
}

func validate(cfg *Config) error {
	if cfg.BaseInterface == "" {
		return errors.New("base_interface is required")
	}

	// If auth is partially configured, require both issuer and allowed subjects
	if (cfg.Auth.Issuer != "" && len(cfg.Auth.AllowedSubjects) == 0) || (cfg.Auth.Issuer == "" && len(cfg.Auth.AllowedSubjects) > 0) {
		return errors.New("auth configuration: both auth.issuer and auth.allowed_subjects must be set together")
	}
	return nil
}
