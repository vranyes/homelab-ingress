package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/vranyes/homelab-ingress/controller/internal/router"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = discoveryv1.AddToScheme(s)
	return s
}

type mockRouterClient struct {
	createCalls []mockCreateCall
	updateCalls []mockUpdateCall
	deleteCalls []string
	getCalls    []string
	listCalls   int
	getResult   *router.ServiceEntry
	getErr      error
	createErr   error
	updateErr   error
	deleteErr   error
	listResult  []router.ServiceEntry
	listErr     error
}

type mockCreateCall struct {
	key   string
	ports []router.PortMapping
}

type mockUpdateCall struct {
	key   string
	ports []router.PortMapping
}

func (m *mockRouterClient) Create(_ context.Context, key string, ports []router.PortMapping) (*router.ServiceEntry, error) {
	m.createCalls = append(m.createCalls, mockCreateCall{key, ports})
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &router.ServiceEntry{ServiceKey: key, AssignedIP: "192.168.1.100", Ports: ports}, nil
}

func (m *mockRouterClient) Update(_ context.Context, key string, ports []router.PortMapping) error {
	m.updateCalls = append(m.updateCalls, mockUpdateCall{key, ports})
	return m.updateErr
}

func (m *mockRouterClient) Delete(_ context.Context, key string) error {
	m.deleteCalls = append(m.deleteCalls, key)
	return m.deleteErr
}

func (m *mockRouterClient) Get(_ context.Context, key string) (*router.ServiceEntry, error) {
	m.getCalls = append(m.getCalls, key)
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.getResult, nil
}

func (m *mockRouterClient) List(_ context.Context) ([]router.ServiceEntry, error) {
	m.listCalls++
	return m.listResult, m.listErr
}

func (m *mockRouterClient) Health(_ context.Context) (*router.HealthStatus, error) {
	return &router.HealthStatus{Healthy: true}, nil
}

func (m *mockRouterClient) Close() error { return nil }

func newTestServiceReconciler(t *testing.T, objs ...client.Object) (*ServiceReconciler, *mockRouterClient) {
	t.Helper()
	scheme := newScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&corev1.Service{}).Build()
	rc := &mockRouterClient{}
	return &ServiceReconciler{Client: fc, RouterClient: rc}, rc
}

func testService(ns, name string, selector map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeLoadBalancer,
			ClusterIP: "10.96.0.10",
			Selector:  selector,
			Ports: []corev1.ServicePort{
				{Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func testReadyPod(ns, name string, labels map[string]string, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func TestServiceReconciler_CreatesLoadBalancer(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})
	pod := testReadyPod("default", "myapp-pod", map[string]string{"app": "myapp"}, "10.244.0.5")

	r, rc := newTestServiceReconciler(t, svc, pod)

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected empty result, got %+v", result)
	}

	if len(rc.createCalls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(rc.createCalls))
	}
	if rc.createCalls[0].key != "default/myapp" {
		t.Errorf("expected key default/myapp, got %q", rc.createCalls[0].key)
	}
	if len(rc.createCalls[0].ports) != 1 {
		t.Errorf("expected 1 port, got %d", len(rc.createCalls[0].ports))
	}
	if len(rc.createCalls[0].ports[0].PodIPs) != 1 || rc.createCalls[0].ports[0].PodIPs[0] != "10.244.0.5" {
		t.Errorf("expected pod IP 10.244.0.5, got %v", rc.createCalls[0].ports[0].PodIPs)
	}

	var updatedSvc corev1.Service
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "myapp"}, &updatedSvc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if !HasFinalizer(&updatedSvc) {
		t.Error("expected finalizer to be added")
	}
	if len(updatedSvc.Status.LoadBalancer.Ingress) != 1 {
		t.Fatalf("expected 1 ingress entry, got %d", len(updatedSvc.Status.LoadBalancer.Ingress))
	}
	if updatedSvc.Status.LoadBalancer.Ingress[0].IP != "192.168.1.100" {
		t.Errorf("expected IP 192.168.1.100, got %q", updatedSvc.Status.LoadBalancer.Ingress[0].IP)
	}
}

func TestServiceReconciler_SkipsNonLoadBalancer(t *testing.T) {
	svc := testService("default", "myapp", nil)
	svc.Spec.Type = corev1.ServiceTypeClusterIP

	r, rc := newTestServiceReconciler(t, svc)

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.createCalls) != 0 {
		t.Error("expected no create call for ClusterIP service")
	}
}

func TestServiceReconciler_SkipsHeadless(t *testing.T) {
	svc := testService("default", "myapp", nil)
	svc.Spec.ClusterIP = corev1.ClusterIPNone

	r, rc := newTestServiceReconciler(t, svc)

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.createCalls) != 0 {
		t.Error("expected no create call for headless service")
	}
}

func TestServiceReconciler_SkipsAnnotated(t *testing.T) {
	svc := testService("default", "myapp", nil)
	svc.Annotations = map[string]string{AnnotationSkip: "true"}

	r, rc := newTestServiceReconciler(t, svc)

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.createCalls) != 0 {
		t.Error("expected no create call for skipped service")
	}
}

func TestServiceReconciler_RequeuesWhenNoPods(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})

	r, _ := newTestServiceReconciler(t, svc)

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s, got %v", result.RequeueAfter)
	}
}

func TestServiceReconciler_UpdatesWhenPortsChanged(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})
	pod := testReadyPod("default", "pod", map[string]string{"app": "myapp"}, "10.244.0.5")
	svc.Finalizers = []string{Finalizer}

	r, rc := newTestServiceReconciler(t, svc, pod)
	rc.getResult = &router.ServiceEntry{
		ServiceKey:   "default/myapp",
		AssignedIP:   "192.168.1.100",
		Ports:        []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 9999, PodIPs: []string{"10.244.0.1"}}},
		DHCPAcquired: true,
	}

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rc.updateCalls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(rc.updateCalls))
	}
}

func TestServiceReconciler_NoUpdateWhenPortsUnchanged(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})
	pod := testReadyPod("default", "pod", map[string]string{"app": "myapp"}, "10.244.0.5")
	svc.Finalizers = []string{Finalizer}

	r, rc := newTestServiceReconciler(t, svc, pod)
	rc.getResult = &router.ServiceEntry{
		ServiceKey:   "default/myapp",
		AssignedIP:   "192.168.1.100",
		Ports:        []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.244.0.5"}}},
		DHCPAcquired: true,
	}

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rc.updateCalls) != 0 {
		t.Errorf("expected no update when ports unchanged, got %d calls", len(rc.updateCalls))
	}
}

func TestServiceReconciler_Delete(t *testing.T) {
	now := metav1.Now()
	svc := testService("default", "myapp", nil)
	svc.Finalizers = []string{Finalizer}
	svc.DeletionTimestamp = &now

	r, rc := newTestServiceReconciler(t, svc)

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rc.deleteCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(rc.deleteCalls))
	}
	if rc.deleteCalls[0] != "default/myapp" {
		t.Errorf("expected delete for default/myapp, got %q", rc.deleteCalls[0])
	}

	var updatedSvc corev1.Service
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "myapp"}, &updatedSvc)
	if err == nil {
		if HasFinalizer(&updatedSvc) {
			t.Error("expected finalizer to be removed")
		}
	}
}

func TestServiceReconciler_DoesNotPatchWhenIPUnchanged(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})
	pod := testReadyPod("default", "pod", map[string]string{"app": "myapp"}, "10.244.0.5")
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.168.1.100"}}

	r, rc := newTestServiceReconciler(t, svc, pod)
	rc.getResult = &router.ServiceEntry{
		ServiceKey:   "default/myapp",
		AssignedIP:   "192.168.1.100",
		Ports:        []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.244.0.5"}}},
		DHCPAcquired: true,
	}

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.updateCalls) != 0 {
		t.Errorf("expected no update, got %d", len(rc.updateCalls))
	}
}

func TestServiceReconciler_AddsFinalizer(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})
	pod := testReadyPod("default", "pod", map[string]string{"app": "myapp"}, "10.244.0.5")

	r, _ := newTestServiceReconciler(t, svc, pod)

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updatedSvc corev1.Service
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "myapp"}, &updatedSvc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if !HasFinalizer(&updatedSvc) {
		t.Error("expected finalizer to be added")
	}
}

func TestServiceReconciler_ServiceNotFound(t *testing.T) {
	r, _ := newTestServiceReconciler(t)

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "nonexistent"}}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServiceReconciler_DeleteRouterError(t *testing.T) {
	now := metav1.Now()
	svc := testService("default", "myapp", nil)
	svc.Finalizers = []string{Finalizer}
	svc.DeletionTimestamp = &now

	r, rc := newTestServiceReconciler(t, svc)
	rc.deleteErr = fmt.Errorf("router unreachable")

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err == nil {
		t.Fatal("expected error from router delete failure")
	}
}

func TestServiceReconciler_GetRouterError(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})
	pod := testReadyPod("default", "pod", map[string]string{"app": "myapp"}, "10.244.0.5")
	svc.Finalizers = []string{Finalizer}

	r, rc := newTestServiceReconciler(t, svc, pod)
	rc.getErr = fmt.Errorf("router unreachable")

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err == nil {
		t.Fatal("expected error from router get failure")
	}
}

func TestServiceReconciler_CreateRouterError(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})
	pod := testReadyPod("default", "pod", map[string]string{"app": "myapp"}, "10.244.0.5")

	r, rc := newTestServiceReconciler(t, svc, pod)
	rc.createErr = fmt.Errorf("router unreachable")

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err == nil {
		t.Fatal("expected error from router create failure")
	}
}

func TestServiceReconciler_UpdateRouterError(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})
	pod := testReadyPod("default", "pod", map[string]string{"app": "myapp"}, "10.244.0.5")
	svc.Finalizers = []string{Finalizer}

	r, rc := newTestServiceReconciler(t, svc, pod)
	rc.getResult = &router.ServiceEntry{
		ServiceKey: "default/myapp",
		AssignedIP: "192.168.1.100",
		Ports:      []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 9999, PodIPs: []string{"10.244.0.1"}}},
	}
	rc.updateErr = fmt.Errorf("router unreachable")

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err == nil {
		t.Fatal("expected error from router update failure")
	}
}

func TestServiceReconciler_NoSelector(t *testing.T) {
	svc := testService("default", "myapp", nil)
	svc.Spec.Selector = nil

	r, _ := newTestServiceReconciler(t, svc)

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s, got %v", result.RequeueAfter)
	}
}

func TestBuildPortMappings_UDP(t *testing.T) {
	svc := &corev1.Service{
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Port: 53, TargetPort: intstr.FromInt(5353), Protocol: corev1.ProtocolUDP},
			},
		},
	}
	pods := []corev1.Pod{
		{Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.42.0.10"}},
	}
	mappings := buildPortMappings(svc, []string{pods[0].Status.PodIP}, nil)
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}
	if mappings[0].Protocol != "udp" {
		t.Errorf("expected udp, got %q", mappings[0].Protocol)
	}
}

func TestBuildPortMappings_MultiplePorts(t *testing.T) {
	svc := &corev1.Service{
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
				{Port: 443, TargetPort: intstr.FromInt(8443), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	pods := []corev1.Pod{
		{Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.42.0.10"}},
		{Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.42.0.11"}},
	}
	mappings := buildPortMappings(svc, []string{pods[0].Status.PodIP, pods[1].Status.PodIP}, nil)
	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(mappings))
	}
	if len(mappings[0].PodIPs) != 2 {
		t.Errorf("expected 2 pod IPs, got %d", len(mappings[0].PodIPs))
	}
}

func TestServiceReconciler_NoPatchWhenNoIP(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})
	pod := testReadyPod("default", "pod", map[string]string{"app": "myapp"}, "10.244.0.5")

	r, rc := newTestServiceReconciler(t, svc, pod)
	rc.getResult = &router.ServiceEntry{
		ServiceKey:   "default/myapp",
		AssignedIP:   "",
		Ports:        []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.244.0.5"}}},
		DHCPAcquired: false,
	}

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updatedSvc corev1.Service
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "myapp"}, &updatedSvc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if len(updatedSvc.Status.LoadBalancer.Ingress) != 0 {
		t.Errorf("expected no ingress when IP empty, got %d entries", len(updatedSvc.Status.LoadBalancer.Ingress))
	}
}

func TestServiceReconciler_UpdateError(t *testing.T) {
	svc := testService("default", "myapp", map[string]string{"app": "myapp"})
	pod := testReadyPod("default", "pod", map[string]string{"app": "myapp"}, "10.244.0.5")
	svc.Finalizers = []string{Finalizer}

	r, rc := newTestServiceReconciler(t, svc, pod)
	rc.getResult = &router.ServiceEntry{
		ServiceKey:   "default/myapp",
		AssignedIP:   "192.168.1.100",
		Ports:        []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 9999, PodIPs: []string{"10.244.0.1"}}},
		DHCPAcquired: true,
	}
	rc.updateErr = fmt.Errorf("router down")

	ctx := log.IntoContext(context.Background(), log.FromContext(context.Background()))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "myapp"}}
	_, err := r.Reconcile(ctx, req)
	if err == nil {
		t.Fatal("expected error from update failure")
	}
}

func TestPortsChanged(t *testing.T) {
	tests := []struct {
		name     string
		existing []router.PortMapping
		desired  []router.PortMapping
		want     bool
	}{
		{
			name:     "same ports",
			existing: []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			desired:  []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			want:     false,
		},
		{
			name:     "different length",
			existing: []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			desired:  []router.PortMapping{},
			want:     true,
		},
		{
			name:     "different protocol",
			existing: []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			desired:  []router.PortMapping{{Protocol: "udp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			want:     true,
		},
		{
			name:     "different pod IPs",
			existing: []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			desired:  []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1", "10.0.0.2"}}},
			want:     true,
		},
		{
			name:     "different port number",
			existing: []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			desired:  []router.PortMapping{{Protocol: "tcp", ExternalPort: 443, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			want:     true,
		},
		{
			name:     "different pod port",
			existing: []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			desired:  []router.PortMapping{{Protocol: "tcp", ExternalPort: 80, PodPort: 9090, PodIPs: []string{"10.0.0.1"}}},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := portsChanged(tt.existing, tt.desired)
			if got != tt.want {
				t.Errorf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestSplitServiceKey(t *testing.T) {
	tests := []struct {
		key  string
		want serviceKeyParts
	}{
		{"default/myapp", serviceKeyParts{"default", "myapp"}},
		{"kube-system/coredns", serviceKeyParts{"kube-system", "coredns"}},
		{"a/b", serviceKeyParts{"a", "b"}},
		{"noslash", serviceKeyParts{"", ""}},
		{"", serviceKeyParts{"", ""}},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := splitServiceKey(tt.key)
			if got != tt.want {
				t.Errorf("expected %+v, got %+v", tt.want, got)
			}
		})
	}
}

func TestStringSliceEqual(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{[]string{"a", "b"}, []string{"a", "b"}, true},
		{[]string{"a", "b"}, []string{"b", "a"}, true},
		{[]string{"a"}, []string{"a", "b"}, false},
		{nil, nil, true},
		{[]string{}, []string{}, true},
		{[]string{"a"}, []string{"b"}, false},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			if got := stringSliceEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("expected %v, got %v", tt.want, got)
			}
		})
	}
}
