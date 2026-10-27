// Package metrics exposes the daemon's Prometheus metrics.
//
// Metric inventory:
//
//	homelab_ingress_services_total            gauge, number of services in state
//	homelab_ingress_nft_apply_errors_total    counter{op}, nftables apply failures (op: add/remove/refresh)
//	homelab_ingress_geo_db_age_seconds        gauge, age of the country-database file as of the last reload
//	homelab_ingress_dhcp_lease_expiry_seconds gauge{service}, Unix timestamp at which the DHCP lease expires (0 = unknown)
//	homelab_ingress_reconcile_total           counter{manager,result}, reconcile loop outcomes
//
// All series are registered on the default registry; Start serves them on
// /metrics. An empty listen address disables the endpoint (returns nil
// immediately) so a config without metrics never breaks startup.
package metrics

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Apply ops for NftApplyErrors.
const (
	OpAdd     = "add"
	OpRemove  = "remove"
	OpRefresh = "refresh"
)

// Reconcile results for ReconcileTotal.
const (
	ResultSuccess = "success"
	ResultError   = "error"
)

var (
	// ServicesTotal is the number of services currently in state.
	ServicesTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "homelab_ingress_services_total",
		Help: "Number of load-balancer services currently in daemon state.",
	})
	// NftApplyErrors counts nftables apply failures by operation.
	NftApplyErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "homelab_ingress_nft_apply_errors_total",
		Help: "Total nftables apply failures by operation (add/remove/refresh).",
	}, []string{"op"})
	// GeoDBAgeSeconds is the age of the country-database file as of the
	// last geo reload. -1 means no successful reload has happened yet.
	GeoDBAgeSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "homelab_ingress_geo_db_age_seconds",
		Help: "Age in seconds of the geo country-database file as of the last reload (-1 when unknown).",
	})
	// DHCPLeaseExpirySeconds is the Unix timestamp at which the DHCP lease
	// for a service expires (0 when unknown). Deleted when the service goes away.
	DHCPLeaseExpirySeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "homelab_ingress_dhcp_lease_expiry_seconds",
		Help: "Unix timestamp at which the service DHCP lease expires (0 when unknown).",
	}, []string{"service"})
	// ReconcileTotal counts reconcile loop outcomes by manager and result.
	ReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "homelab_ingress_reconcile_total",
		Help: "Total reconcile loop runs by manager (router/geo/dhcp/nftables) and result (success/error).",
	}, []string{"manager", "result"})
)

func init() {
	prometheus.MustRegister(ServicesTotal, NftApplyErrors, GeoDBAgeSeconds, DHCPLeaseExpirySeconds, ReconcileTotal)
	GeoDBAgeSeconds.Set(-1)
}

// Start serves /metrics on addr in a background goroutine. An empty addr
// disables the endpoint and returns immediately.
func Start(addr string, logger *slog.Logger) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if logger != nil {
			logger.Info("metrics server started", "listen", addr)
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			if logger != nil {
				logger.Error("metrics server error", "error", err)
			}
		}
	}()
}

// IncNftApplyError records an nftables apply failure for op (OpAdd/OpRemove/OpRefresh).
func IncNftApplyError(op string) {
	NftApplyErrors.WithLabelValues(op).Inc()
}

// ObserveReconcile records a reconcile loop outcome for manager.
func ObserveReconcile(manager string, err error) {
	result := ResultSuccess
	if err != nil {
		result = ResultError
	}
	ReconcileTotal.WithLabelValues(manager, result).Inc()
}

// SetServicesTotal records the number of services in state.
func SetServicesTotal(n int) {
	ServicesTotal.Set(float64(n))
}

// SetGeoDBAgeSeconds records the country-database file age in seconds.
func SetGeoDBAgeSeconds(seconds float64) {
	GeoDBAgeSeconds.Set(seconds)
}

// SetDHCPLeaseExpiry records the Unix expiry timestamp for a service lease.
func SetDHCPLeaseExpiry(service string, expiryUnix int64) {
	DHCPLeaseExpirySeconds.WithLabelValues(service).Set(float64(expiryUnix))
}

// DeleteDHCPLeaseExpiry drops the expiry series for a removed service.
func DeleteDHCPLeaseExpiry(service string) {
	DHCPLeaseExpirySeconds.DeleteLabelValues(service)
}
