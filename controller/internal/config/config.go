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
	// authenticate requests to the router service. When empty, no auth header
	// is sent.
	TokenPath string
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
	return nil
}
