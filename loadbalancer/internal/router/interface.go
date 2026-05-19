package router

import (
	"errors"
	"log/slog"
	"time"

	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
)

type Implementor interface {
	AddRules(service state.LBService) error
	RemoveRules(serviceKey string) error
	ListRules() ([]string, error)
}

type Manager struct {
	Implementor
	logger *slog.Logger
	st     state.State
}

func NewManager(impl Implementor, st state.State, logger *slog.Logger) *Manager {
	return &Manager{
		Implementor: impl,
		st:          st,
		logger:      logger,
	}
}

// Reconcile ensures that the current state of rules matches the desired state.
// If routes are missing, they are added.
// If extraneous routes are present, they are removed.
func (m *Manager) Reconcile(st *state.State) error {
	have, err := m.ListRules()
	if err != nil {
		return err
	}
	var e error
	for _, has := range have {
		if !state.HasService(st, has) {
			if err := m.RemoveRules(has); err != nil {
				e = errors.Join(e, err)
			}
		}
	}
	for _, want := range st.Services {
		if err := m.AddRules(*want); err != nil {
			e = errors.Join(e, err)
		}
	}
	return e
}

// Watch responds to events emitted by the state package when services are added, updated, or removed.
// The channel receives the full LBService struct for added or updated services, and a struct with only
// the ServiceName field populated for removed services.
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

		case <-time.After(30 * time.Second):
			if err := m.Reconcile(&m.st); err != nil {
				m.logger.Error("failed to reconcile state", "error", err)
			}
		}
	}
}
