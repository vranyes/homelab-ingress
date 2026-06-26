//go:build !linux

package nftables

import (
	"log/slog"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

type Manager struct {
	logger *slog.Logger
}

func NewManager(logger *slog.Logger, _ ...Option) *Manager { return &Manager{logger: logger} }

func (m *Manager) AddRules(svc state.LBService) error {
	return validateService(svc)
}

func (m *Manager) RemoveRules(_ string) error { return nil }

func (m *Manager) ListRules() ([]string, error) { return nil, nil }
