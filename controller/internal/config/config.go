package config

import (
	"errors"
	"time"
)

const (
	DefaultRouterAddr   = "localhost:50051"
	DefaultPollInterval = 30 * time.Second
	DefaultMetricsAddr  = ":8080"
)

type Config struct {
	RouterAddr   string
	PollInterval time.Duration
	MetricsAddr  string
	// TokenPath is the path to a file containing a Bearer token used to
	// authenticate requests to the router service.
	TokenPath string
	// CAPath is the path to a CA certificate file used to verify the router
	// service's TLS certificate.
	CAPath string
}

func DefaultConfig() Config {
	return Config{
		RouterAddr:   DefaultRouterAddr,
		PollInterval: DefaultPollInterval,
		MetricsAddr:  DefaultMetricsAddr,
	}
}

func Validate(cfg Config) error {
	if cfg.RouterAddr == "" {
		return errors.New("router-addr is required")
	}
	if cfg.PollInterval <= 0 {
		return errors.New("poll-interval must be positive")
	}
	if cfg.TokenPath == "" {
		return errors.New("token-path is required")
	}
	if cfg.CAPath == "" {
		return errors.New("ca-path is required")
	}
	return nil
}
