//go:build e2e && linux

package e2e_test

import (
	"context"
	"time"

	routerv1 "github.com/vranyes/homelab-ingress/controller/pkg/api/router/v1"
)

// contextWithTimeout returns a fresh context that cancels after `dur`. All
// scenarios call this rather than reaching for context.WithTimeout directly so
// the timeout centralised here stays consistent.
func contextWithTimeout(dur time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dur)
}

// healthReq and friends save allocations in scenarios; the fields of these
// protobuf messages are empty so they are safe to share.
func healthReq() *routerv1.HealthRequest { return &routerv1.HealthRequest{} }

// getLBReq builds a fresh GetLoadBalancer request for the given service key.
func getLBReq(serviceKey string) *routerv1.GetLoadBalancerRequest {
	return &routerv1.GetLoadBalancerRequest{ServiceKey: serviceKey}
}

// listLBReq returns a fresh ListLoadBalancers request.
func listLBReq() *routerv1.ListLoadBalancersRequest { return &routerv1.ListLoadBalancersRequest{} }

// serviceKey returns the canonical `<ns>/<name>` service key used by the
// controller and ingress.
func serviceKey(name string) string { return e2eNamespace + "/" + name }
