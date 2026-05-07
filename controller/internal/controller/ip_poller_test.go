package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/vranyes/homelab-ingress/controller/internal/router"
)

func newTestIPPoller(t *testing.T, rc *mockRouterClient, objs ...client.Object) *IPPoller {
	t.Helper()
	scheme := newScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&corev1.Service{}).Build()
	return &IPPoller{
		Client:       fc,
		RouterClient: rc,
		Interval:     100 * time.Millisecond,
	}
}

func TestIPPoller_UpdatesChangedIP(t *testing.T) {
	svc := testService("default", "myapp", nil)
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.168.1.100"}}

	rc := &mockRouterClient{
		listResult: []router.ServiceEntry{
			{ServiceKey: "default/myapp", AssignedIP: "10.0.0.5"},
		},
	}

	poller := newTestIPPoller(t, rc, svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := poller.poll(log.IntoContext(ctx, log.FromContext(ctx)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updatedSvc corev1.Service
	if err := poller.Get(ctx, client.ObjectKey{Namespace: "default", Name: "myapp"}, &updatedSvc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if len(updatedSvc.Status.LoadBalancer.Ingress) != 1 {
		t.Fatalf("expected 1 ingress, got %d", len(updatedSvc.Status.LoadBalancer.Ingress))
	}
	if updatedSvc.Status.LoadBalancer.Ingress[0].IP != "10.0.0.5" {
		t.Errorf("expected IP 10.0.0.5, got %q", updatedSvc.Status.LoadBalancer.Ingress[0].IP)
	}
}

func TestIPPoller_NoUpdateWhenIPUnchanged(t *testing.T) {
	svc := testService("default", "myapp", nil)
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.168.1.100"}}

	rc := &mockRouterClient{
		listResult: []router.ServiceEntry{
			{ServiceKey: "default/myapp", AssignedIP: "192.168.1.100"},
		},
	}

	poller := newTestIPPoller(t, rc, svc)

	ctx := context.Background()
	err := poller.poll(log.IntoContext(ctx, log.FromContext(ctx)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updatedSvc corev1.Service
	if err := poller.Get(ctx, client.ObjectKey{Namespace: "default", Name: "myapp"}, &updatedSvc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if updatedSvc.Status.LoadBalancer.Ingress[0].IP != "192.168.1.100" {
		t.Errorf("IP should remain 192.168.1.100, got %q", updatedSvc.Status.LoadBalancer.Ingress[0].IP)
	}
}

func TestIPPoller_SkipsPendingDHCP(t *testing.T) {
	svc := testService("default", "myapp", nil)
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.168.1.100"}}

	rc := &mockRouterClient{
		listResult: []router.ServiceEntry{
			{ServiceKey: "default/myapp", AssignedIP: ""},
		},
	}

	poller := newTestIPPoller(t, rc, svc)

	ctx := context.Background()
	err := poller.poll(log.IntoContext(ctx, log.FromContext(ctx)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updatedSvc corev1.Service
	if err := poller.Get(ctx, client.ObjectKey{Namespace: "default", Name: "myapp"}, &updatedSvc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if updatedSvc.Status.LoadBalancer.Ingress[0].IP != "192.168.1.100" {
		t.Errorf("IP should remain unchanged, got %q", updatedSvc.Status.LoadBalancer.Ingress[0].IP)
	}
}

func TestIPPoller_SkipsMissingService(t *testing.T) {
	rc := &mockRouterClient{
		listResult: []router.ServiceEntry{
			{ServiceKey: "default/nonexistent", AssignedIP: "10.0.0.5"},
		},
	}

	poller := newTestIPPoller(t, rc)

	ctx := context.Background()
	err := poller.poll(log.IntoContext(ctx, log.FromContext(ctx)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIPPoller_SkipsNonLoadBalancerService(t *testing.T) {
	svc := testService("default", "myapp", nil)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.168.1.100"}}

	rc := &mockRouterClient{
		listResult: []router.ServiceEntry{
			{ServiceKey: "default/myapp", AssignedIP: "10.0.0.5"},
		},
	}

	poller := newTestIPPoller(t, rc, svc)

	ctx := context.Background()
	err := poller.poll(log.IntoContext(ctx, log.FromContext(ctx)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updatedSvc corev1.Service
	if err := poller.Get(ctx, client.ObjectKey{Namespace: "default", Name: "myapp"}, &updatedSvc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if updatedSvc.Status.LoadBalancer.Ingress[0].IP != "192.168.1.100" {
		t.Errorf("IP should remain unchanged for non-LB service, got %q", updatedSvc.Status.LoadBalancer.Ingress[0].IP)
	}
}

func TestIPPoller_HandlesMultipleEntries(t *testing.T) {
	svc1 := testService("default", "app1", nil)
	svc1.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.168.1.100"}}

	svc2 := testService("default", "app2", nil)
	svc2.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.168.1.200"}}

	rc := &mockRouterClient{
		listResult: []router.ServiceEntry{
			{ServiceKey: "default/app1", AssignedIP: "10.0.0.5"},
			{ServiceKey: "default/app2", AssignedIP: "10.0.0.6"},
		},
	}

	poller := newTestIPPoller(t, rc, svc1, svc2)

	ctx := context.Background()
	err := poller.poll(log.IntoContext(ctx, log.FromContext(ctx)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var s1 corev1.Service
	if err := poller.Get(ctx, client.ObjectKey{Namespace: "default", Name: "app1"}, &s1); err != nil {
		t.Fatalf("get app1: %v", err)
	}
	if s1.Status.LoadBalancer.Ingress[0].IP != "10.0.0.5" {
		t.Errorf("expected app1 IP 10.0.0.5, got %q", s1.Status.LoadBalancer.Ingress[0].IP)
	}

	var s2 corev1.Service
	if err := poller.Get(ctx, client.ObjectKey{Namespace: "default", Name: "app2"}, &s2); err != nil {
		t.Fatalf("get app2: %v", err)
	}
	if s2.Status.LoadBalancer.Ingress[0].IP != "10.0.0.6" {
		t.Errorf("expected app2 IP 10.0.0.6, got %q", s2.Status.LoadBalancer.Ingress[0].IP)
	}
}

func TestIPPoller_ListError(t *testing.T) {
	rc := &mockRouterClient{
		listErr: context.DeadlineExceeded,
	}

	poller := newTestIPPoller(t, rc)

	ctx := context.Background()
	err := poller.poll(log.IntoContext(ctx, log.FromContext(ctx)))
	if err == nil {
		t.Fatal("expected error from list failure")
	}
}

func TestIPPoller_NeedLeaderElection(t *testing.T) {
	poller := &IPPoller{}
	if !poller.NeedLeaderElection() {
		t.Error("expected NeedLeaderElection to return true")
	}
}

func TestIPPoller_StartStopsOnContextCancel(t *testing.T) {
	rc := &mockRouterClient{
		listResult: []router.ServiceEntry{},
	}
	poller := &IPPoller{
		RouterClient: rc,
		Interval:     10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- poller.Start(log.IntoContext(ctx, log.FromContext(ctx)))
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop within timeout")
	}
}
