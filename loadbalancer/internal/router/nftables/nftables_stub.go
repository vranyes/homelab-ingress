//go:build !linux

package nftables

import (
	"log/slog"

	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
)

// Manager is a no-op firewall manager for non-Linux platforms.
type Manager struct {
	logger *slog.Logger
}

func NewManager() *Manager                              { return NewManagerWithLogger(slog.Default()) }
func NewManagerWithLogger(logger *slog.Logger) *Manager { return &Manager{logger: logger} }
func NewNoOpManager() *Manager                          { return NewManager() }

func (m *Manager) AddRules(svc state.LBService) error {
	return validateService(svc)
}

func (m *Manager) RemoveRules(_ string) error { return nil }
