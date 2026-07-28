package router

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

type Implementor interface {
	AddRules(service state.LBService) error
	RemoveRules(serviceKey string) error
	ListRules() ([]string, error)
}

type Manager struct {
	Implementor
	logger *slog.Logger
	st     *state.State
}

func NewManager(impl Implementor, st *state.State, logger *slog.Logger) *Manager {
	return &Manager{
		Implementor: impl,
		st:          st,
		logger:      logger,
	}
}

func (m *Manager) Reconcile() error {
	have, err := m.ListRules()
	if err != nil {
		return err
	}
	snap := m.st.Clone()
	var e error
	for _, has := range have {
		if !snap.HasService(has) {
			if err := m.RemoveRules(has); err != nil {
				e = errors.Join(e, err)
			}
		}
	}
	for _, want := range snap.Services {
		if err := m.AddRules(*want); err != nil {
			e = errors.Join(e, fmt.Errorf("failed to add rules for service %s: %w", want.ServiceName, err))
		}
	}
	return e
}

func (m *Manager) Run(notif <-chan state.LBNotification) {
	for {
		select {
		case event := <-notif:
			switch event.Type {
			case state.CreateEvent:
				m.logger.Info("received create event", "service", event.Service)
				if err := m.AddRules(event.Service); err != nil {
					m.logger.Error("failed to add rules for service", "service", event.Service, "error", err)
				}
			case state.UpdateEvent:
				m.logger.Info("received update event", "service", event.Service)
				if err := m.AddRules(event.Service); err != nil {
					m.logger.Error("failed to update rules for service", "service", event.Service, "error", err)
				}
			case state.DeleteEvent:
				m.logger.Info("received delete event", "service", event.Service)
				if err := m.RemoveRules(event.Service.ServiceName); err != nil {
					m.logger.Error("failed to remove rules for service", "service", event.Service, "error", err)
				}
			}

		case <-time.After(5 * time.Minute):
			if err := m.Reconcile(); err != nil {
				m.logger.Error("failed to reconcile state", "error", err)
			}
		}
	}
}
