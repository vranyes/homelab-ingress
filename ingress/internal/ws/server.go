//go:build linux

package ws

import (
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/vranyes/homelab-ingress/ingress/internal/auth"
	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

type IPEvent struct {
	Event        string `json:"event"`
	ServiceKey   string `json:"service_key"`
	AssignedIP   string `json:"assigned_ip,omitempty"`
	DHCPAcquired bool   `json:"dhcp_acquired,omitempty"`
}

type Server struct {
	addr     string
	verifier *auth.TokenVerifier
	st       *state.State
	tlsCfg   *tls.Config
	logger   *slog.Logger
}

func NewServer(addr string, verifier *auth.TokenVerifier, st *state.State, tlsCfg *tls.Config, logger *slog.Logger) *Server {
	return &Server{
		addr:     addr,
		verifier: verifier,
		st:       st,
		tlsCfg:   tlsCfg,
		logger:   logger,
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.Handle("/events", s.authMiddleware(websocket.Handler(s.handleEvents)))
	srv := &http.Server{
		Addr:      s.addr,
		Handler:   mux,
		TLSConfig: s.tlsCfg,
	}

	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.logger.Info("WebSocket server starting", "addr", s.addr)
	return srv.ServeTLS(listener, "", "")
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := auth.ExtractBearerTokenHTTP(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if _, err := s.verifier.Verify(r.Context(), raw); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleEvents(ws *websocket.Conn) {
	defer ws.Close()
	s.logger.Info("WebSocket client connected", "remote", ws.RemoteAddr())

	notif := s.st.Subscribe()
	defer func() {
		websocket.Message.Send(ws, `{"event":"shutdown"}`)
		s.st.Unsubscribe(notif)
	}()

	tracker := newIPTracker()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case ev, ok := <-notif:
			if !ok {
				return
			}
			msg := s.buildEvent(ev, tracker)
			if msg == nil {
				continue
			}
			data, err := json.Marshal(msg)
			if err != nil {
				s.logger.Error("failed to marshal event", "error", err)
				continue
			}
			if err := websocket.Message.Send(ws, string(data)); err != nil {
				s.logger.Debug("WebSocket send failed, client disconnected", "error", err)
				return
			}
		case <-ping.C:
			if err := websocket.Message.Send(ws, `{"event":"ping"}`); err != nil {
				s.logger.Debug("WebSocket ping failed, client disconnected", "error", err)
				return
			}
		}
	}
}

func (s *Server) buildEvent(notif state.LBNotification, tracker *ipTracker) *IPEvent {
	switch notif.Type {
	case state.DeleteEvent:
		tracker.remove(notif.Service.ServiceName)
		return &IPEvent{
			Event:      "service_deleted",
			ServiceKey: notif.Service.ServiceName,
		}
	case state.CreateEvent, state.UpdateEvent:
		var ip string
		if notif.Service.SourceAddress != nil {
			ip = notif.Service.SourceAddress.String()
		}
		last := tracker.get(notif.Service.ServiceName)
		if ip == "" && last == "" {
			return nil
		}
		if ip == last {
			return nil
		}
		tracker.set(notif.Service.ServiceName, ip)
		if ip == "" {
			return nil
		}
		dhcp := false
		if notif.Service.DHCPAcquired != nil {
			dhcp = *notif.Service.DHCPAcquired
		}
		return &IPEvent{
			Event:        "ip_assigned",
			ServiceKey:   notif.Service.ServiceName,
			AssignedIP:   ip,
			DHCPAcquired: dhcp,
		}
	}
	return nil
}

// ipTracker tracks the last known IP per service key to detect changes.
type ipTracker struct {
	mu  sync.Mutex
	ips map[string]string
}

func newIPTracker() *ipTracker {
	return &ipTracker{ips: make(map[string]string)}
}

func (t *ipTracker) get(key string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ips[key]
}

func (t *ipTracker) set(key, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ips[key] = ip
}

func (t *ipTracker) remove(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.ips, key)
}
