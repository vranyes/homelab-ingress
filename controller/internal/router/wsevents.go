package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/log"
)

type IPEvent struct {
	Event        string `json:"event"`
	ServiceKey   string `json:"service_key"`
	AssignedIP   string `json:"assigned_ip,omitempty"`
	DHCPAcquired bool   `json:"dhcp_acquired,omitempty"`
}

type WSEventClient struct {
	client.Client
	url       string
	tokenPath string
	caPath    string
}

func NewWSEventClient(cl client.Client, url, tokenPath, caPath string) *WSEventClient {
	return &WSEventClient{
		Client:    cl,
		url:       url,
		tokenPath: tokenPath,
		caPath:    caPath,
	}
}

func (c *WSEventClient) NeedLeaderElection() bool {
	return true
}

func (c *WSEventClient) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("ws-events")

	tlsCfg, err := c.tlsConfig()
	if err != nil {
		return fmt.Errorf("build TLS config: %w", err)
	}

	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			log.Info("stopping WebSocket event client")
			return nil
		default:
		}

		token, err := c.readToken()
		if err != nil {
			log.Error(err, "failed to read token, retrying")
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		config, err := websocket.NewConfig(c.url, "http://localhost")
		if err != nil {
			log.Error(err, "invalid WebSocket URL")
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		config.Header = http.Header{
			"Authorization": {"Bearer " + token},
		}
		config.TlsConfig = tlsCfg

		ws, err := websocket.DialConfig(config)
		if err != nil {
			log.V(1).Info("WebSocket connection failed, retrying", "error", err)
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		log.Info("WebSocket connected", "url", c.url)
		backoff = time.Second

		if err := c.readLoop(ctx, ws); err != nil {
			log.V(1).Info("WebSocket read loop ended", "error", err)
		}
		ws.Close()
	}
}

func (c *WSEventClient) readLoop(ctx context.Context, ws *websocket.Conn) error {
	ws.SetDeadline(time.Time{})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		var raw string
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			return err
		}

		var ev IPEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			continue
		}

		switch ev.Event {
		case "ping", "shutdown":
			continue
		case "ip_assigned":
			c.handleIPAssigned(ctx, ev)
		case "service_deleted":
			c.handleServiceDeleted(ctx, ev)
		}
	}
}

func (c *WSEventClient) handleIPAssigned(ctx context.Context, ev IPEvent) {
	log := ctrl.Log.WithName("ws-events")

	ns, name := splitServiceKey(ev.ServiceKey)
	if ns == "" || name == "" {
		return
	}

	var svc corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &svc); err != nil {
		log.V(1).Info("service not found for WebSocket event", "service", ev.ServiceKey)
		return
	}

	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return
	}

	currentIP := ""
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		currentIP = svc.Status.LoadBalancer.Ingress[0].IP
	}
	if currentIP == ev.AssignedIP {
		return
	}

	log.Info("WebSocket: IP assigned", "service", ev.ServiceKey, "ip", ev.AssignedIP)
	patch := client.MergeFrom(svc.DeepCopy())
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ev.AssignedIP}}
	if err := c.Status().Patch(ctx, &svc, patch); err != nil {
		log.Error(err, "failed to patch service status from WebSocket event", "service", ev.ServiceKey)
	}
}

func (c *WSEventClient) handleServiceDeleted(ctx context.Context, ev IPEvent) {
	log := ctrl.Log.WithName("ws-events")

	ns, name := splitServiceKey(ev.ServiceKey)
	if ns == "" || name == "" {
		return
	}

	var svc corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &svc); err != nil {
		return
	}

	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		return
	}

	log.Info("WebSocket: service deleted, clearing IP", "service", ev.ServiceKey)
	patch := client.MergeFrom(svc.DeepCopy())
	svc.Status.LoadBalancer.Ingress = nil
	if err := c.Status().Patch(ctx, &svc, patch); err != nil {
		log.Error(err, "failed to clear service status from WebSocket event", "service", ev.ServiceKey)
	}
}

func (c *WSEventClient) tlsConfig() (*tls.Config, error) {
	pool := x509.NewCertPool()
	caData, err := os.ReadFile(c.caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", c.caPath, err)
	}
	if !pool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("no valid CA cert in %q", c.caPath)
	}
	return &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}, nil
}

func (c *WSEventClient) readToken() (string, error) {
	data, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return "", fmt.Errorf("read token file %q: %w", c.tokenPath, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	if next < current {
		return max
	}
	return next
}

func splitServiceKey(key string) (namespace, name string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return "", ""
}
