// Backend health series for active TCP probing (ADR 0009).
//
//	homelab_ingress_backend_up{service,addr,port}          gauge, 1 when the backend is healthy, 0 when down
//	homelab_ingress_backend_probes_total{service,result}   counter, TCP probes by service and result (success/failure)
//
// Like the rest of the package, series are registered on the default
// registry. The health checker keeps them current; tests and operators can
// also read Checker.Stats for the same information without scraping.
package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Backend probe results for BackendProbesTotal.
const (
	ProbeResultSuccess = "success"
	ProbeResultFailure = "failure"
)

var (
	// BackendUp is 1 when the backend is currently considered healthy and
	// 0 when it is down. Unknown (never probed) backends have no series,
	// which rule planning treats as healthy (fail-open).
	BackendUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "homelab_ingress_backend_up",
		Help: "Backend health as seen by active TCP probing (1 healthy, 0 down).",
	}, []string{"service", "addr", "port"})
	// BackendProbesTotal counts TCP health-check probes by service and
	// result (success/failure).
	BackendProbesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "homelab_ingress_backend_probes_total",
		Help: "Total active TCP health-check probes by service and result.",
	}, []string{"service", "result"})
)

func init() {
	prometheus.MustRegister(BackendUp, BackendProbesTotal)
}

// SetBackendUp records the current health of a backend.
func SetBackendUp(service, addr string, port uint16, healthy bool) {
	v := 0.0
	if healthy {
		v = 1
	}
	BackendUp.WithLabelValues(service, addr, strconv.Itoa(int(port))).Set(v)
}

// IncBackendProbe records one probe outcome for a service.
func IncBackendProbe(service string, ok bool) {
	result := ProbeResultFailure
	if ok {
		result = ProbeResultSuccess
	}
	BackendProbesTotal.WithLabelValues(service, result).Inc()
}

// DeleteBackendUp drops the health series for a backend that no longer
// exists in State (service removed or backend decommissioned).
func DeleteBackendUp(service, addr string, port uint16) {
	BackendUp.DeleteLabelValues(service, addr, strconv.Itoa(int(port)))
}
