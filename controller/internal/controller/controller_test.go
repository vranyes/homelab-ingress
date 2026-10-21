package controller

import (
	"context"
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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vranyes/homelab-ingress/controller/internal/router"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(s); err != nil {
		t.Fatalf("discoveryv1 scheme: %v", err)
	}
	return s
}

func strptr(s string) *string  { return &s }
func int32ptr(i int32) *int32  { return &i }
func boolptr(b bool) *bool     { return &b }
func testCtx() context.Context { return log.IntoContext(context.Background(), ctrl.Log) }
func lbClass() *string         { return strptr("homelab-ingress") }
func otherClass() *string      { return strptr("other-class") }

// stubRouter is a fake router.Client. NotFound on Get drives the
// create branch; cannedIP is handed out on Create.
type stubRouter struct {
	entries        map[string]*router.ServiceEntry
	cannedIP       string
	createCalls    []string
	updateCalls    []string
	deleteCalls    []string
	lastCreateGeos []string
	lastUpdateGeos []string
	updateErr      error
}

func newStubRouter(ip string) *stubRouter {
	return &stubRouter{entries: map[string]*router.ServiceEntry{}, cannedIP: ip}
}

func (s *stubRouter) Create(_ context.Context, key string, ports []router.PortMapping, geos []string) (*router.ServiceEntry, error) {
	s.createCalls = append(s.createCalls, key)
	s.lastCreateGeos = append([]string(nil), geos...)
	e := &router.ServiceEntry{ServiceKey: key, AssignedIP: s.cannedIP, Ports: ports, AllowedGeos: geos}
	s.entries[key] = e
	return e, nil
}

func (s *stubRouter) Update(_ context.Context, key string, ports []router.PortMapping, geos []string) error {
	s.updateCalls = append(s.updateCalls, key)
	s.lastUpdateGeos = append([]string(nil), geos...)
	if s.updateErr != nil {
		return s.updateErr
	}
	if e, ok := s.entries[key]; ok {
		e.Ports = ports
		e.AllowedGeos = geos
	}
	return nil
}

func (s *stubRouter) Delete(_ context.Context, key string) error {
	s.deleteCalls = append(s.deleteCalls, key)
	delete(s.entries, key)
	return nil
}

func (s *stubRouter) Get(_ context.Context, key string) (*router.ServiceEntry, error) {
	if e, ok := s.entries[key]; ok {
		return e, nil
	}
	return nil, status.Error(codes.NotFound, "service not found")
}

func (s *stubRouter) List(_ context.Context) ([]router.ServiceEntry, error) {
	out := make([]router.ServiceEntry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, *e)
	}
	return out, nil
}

func (s *stubRouter) Health(_ context.Context) (*router.HealthStatus, error) {
	return &router.HealthStatus{Healthy: true}, nil
}

func (s *stubRouter) Close() error { return nil }

func TestAnnotations(t *testing.T) {
	svc := &corev1.Service{}
	if ShouldSkip(svc) {
		t.Fatalf("nil annotations should not skip")
	}
	svc.Annotations = map[string]string{AnnotationSkip: "true"}
	if !ShouldSkip(svc) {
		t.Fatalf("skip=true should skip")
	}
	svc.Annotations[AnnotationSkip] = "false"
	if ShouldSkip(svc) {
		t.Fatalf("skip=false should not skip")
	}

	if HasFinalizer(svc) {
		t.Fatalf("fresh service should not have finalizer")
	}
	AddFinalizer(svc)
	AddFinalizer(svc) // idempotent
	if !HasFinalizer(svc) || len(svc.Finalizers) != 1 {
		t.Fatalf("finalizer add not idempotent: %+v", svc.Finalizers)
	}
	RemoveFinalizer(svc)
	if HasFinalizer(svc) {
		t.Fatalf("finalizer not removed")
	}
}

func TestSplitServiceKey(t *testing.T) {
	p := splitServiceKey("ns/name")
	if p.namespace != "ns" || p.name != "name" {
		t.Fatalf("got %+v", p)
	}
	// First slash wins; k8s names cannot contain '/', so anything else is
	// malformed input that must not match a real service.
	if p := splitServiceKey("noslash"); p.namespace != "" || p.name != "" {
		t.Fatalf("expected empty parts, got %+v", p)
	}
	if p := splitServiceKey(""); p.namespace != "" || p.name != "" {
		t.Fatalf("expected empty parts, got %+v", p)
	}
}

func TestPortsChanged(t *testing.T) {
	mk := func(addrs ...string) router.PortMapping {
		dsts := make([]router.Destination, len(addrs))
		for i, addr := range addrs {
			dsts[i] = router.Destination{Addr: addr, Port: 8080}
		}
		return router.PortMapping{Protocol: "tcp", ExternalPort: 80, Destinations: dsts}
	}
	a := []router.PortMapping{mk("10.0.0.1", "10.0.0.2")}
	// Same set, different destination order: no change (map-based comparison).
	if portsChanged(a, []router.PortMapping{mk("10.0.0.2", "10.0.0.1")}) {
		t.Fatalf("destination-order-only difference should not count as changed")
	}
	if !portsChanged(a, []router.PortMapping{mk("10.0.0.1")}) {
		t.Fatalf("removed destination should count as changed")
	}
	changed := []router.PortMapping{{Protocol: "udp", ExternalPort: 80,
		Destinations: []router.Destination{{Addr: "10.0.0.1", Port: 8080}, {Addr: "10.0.0.2", Port: 8080}}}}
	if !portsChanged(a, changed) {
		t.Fatalf("protocol change should count as changed")
	}
	changed = []router.PortMapping{{Protocol: "tcp", ExternalPort: 443,
		Destinations: []router.Destination{{Addr: "10.0.0.1", Port: 8080}, {Addr: "10.0.0.2", Port: 8080}}}}
	if !portsChanged(a, changed) {
		t.Fatalf("external port change should count as changed")
	}
	if !portsChanged(a, []router.PortMapping{mk("10.0.0.1", "10.0.0.2"), mk("10.0.0.3")}) {
		t.Fatalf("added mapping should count as changed")
	}
}

func TestBuildPortMappings(t *testing.T) {
	targets := []target{
		{Name: "http", IP: "10.0.0.1", Port: 8080},
		{Name: "http", IP: "10.0.0.2", Port: 8080},
		{Name: "metrics", IP: "10.0.0.1", Port: 9090},
	}
	svc := &corev1.Service{
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
			{Name: "metrics", Port: 9090, TargetPort: intstr.FromString("metrics"), Protocol: corev1.ProtocolTCP},
			{Name: "unresolved", Port: 443, TargetPort: intstr.FromString("nope"), Protocol: corev1.ProtocolTCP},
		}},
	}
	maps := buildPortMappings(svc, targets)
	if len(maps) != 3 {
		t.Fatalf("expected 3 mappings, got %d", len(maps))
	}
	if len(maps[0].Destinations) != 2 {
		t.Fatalf("http should resolve 2 destinations, got %+v", maps[0].Destinations)
	}
	if maps[1].Destinations[0].Port != 9090 {
		t.Fatalf("named port should resolve to 9090, got %+v", maps[1].Destinations[0])
	}
	// Unresolvable named target port with no matching endpoints yields no
	// destinations; the reconciler requeues earlier on zero ready endpoints.
	if len(maps[2].Destinations) != 0 {
		t.Fatalf("unresolvable named port should yield no destinations, got %+v", maps[2].Destinations)
	}
}

func endpointSlice(ns, sliceName, svcName, portName string, port int32, ready bool, ips ...string) *discoveryv1.EndpointSlice {
	eps := make([]discoveryv1.Endpoint, 0, len(ips))
	for _, ip := range ips {
		eps = append(eps, discoveryv1.Endpoint{
			Addresses:  []string{ip},
			Conditions: discoveryv1.EndpointConditions{Ready: boolptr(ready)},
		})
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sliceName,
			Namespace: ns,
			Labels:    map[string]string{"kubernetes.io/service-name": svcName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   eps,
		Ports: []discoveryv1.EndpointPort{{
			Name: strptr(portName), Port: int32ptr(port), Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
		}},
	}
}

func TestEndpointInfoForService_ReadyFiltering(t *testing.T) {
	scheme := testScheme(t)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1", "10.0.0.2"),
			endpointSlice("ns", "web-b", "web", "http", 8080, false, "10.0.0.3"),
			endpointSlice("ns", "other-a", "other", "http", 8080, true, "10.0.0.9"),
		).Build()
	r := &ServiceReconciler{Client: cl}
	targets, err := r.endpointInfoForService(testCtx(), svc)
	if err != nil {
		t.Fatalf("endpointInfo: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 ready targets (not-ready + foreign service excluded), got %+v", targets)
	}
	for _, tg := range targets {
		if tg.IP == "10.0.0.3" || tg.IP == "10.0.0.9" {
			t.Fatalf("unexpected target leaked: %+v", tg)
		}
	}
}

func lbService(ns, name string, mutate func(*corev1.Service)) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass: lbClass(),
			ClusterIP:         "10.96.0.1",
			Selector:          map[string]string{"app": name},
			Ports: []corev1.ServicePort{{
				Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	if mutate != nil {
		mutate(svc)
	}
	return svc
}

func doReconcile(t *testing.T, objs []client.Object, rt *stubRouter) (ctrl.Result, *stubRouter, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&corev1.Service{}).Build()
	r := &ServiceReconciler{Client: cl, RouterClient: rt}
	res, err := r.Reconcile(testCtx(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res, rt, cl
}

func getSvc(t *testing.T, cl client.Client) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{}
	if err := cl.Get(testCtx(), types.NamespacedName{Namespace: "ns", Name: "web"}, svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	return svc
}

func TestReconcile_Gating(t *testing.T) {
	newLB := func() *corev1.Service { return lbService("ns", "web", nil) }

	t.Run("ClusterIP ignored", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		svc := newLB()
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		_, _, cl := doReconcile(t, []client.Object{svc}, rt)
		if HasFinalizer(getSvc(t, cl)) || len(rt.createCalls) != 0 {
			t.Fatalf("ClusterIP service must be ignored")
		}
	})

	t.Run("missing ClusterIP ignored", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		svc := newLB()
		svc.Spec.ClusterIP = ""
		_, _, cl := doReconcile(t, []client.Object{svc}, rt)
		if HasFinalizer(getSvc(t, cl)) || len(rt.createCalls) != 0 {
			t.Fatalf("service without ClusterIP must be ignored")
		}
	})

	t.Run("foreign class ignored", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		svc := lbService("ns", "web", func(s *corev1.Service) {
			s.Spec.LoadBalancerClass = otherClass()
		})
		_, _, cl := doReconcile(t, []client.Object{svc}, rt)
		if HasFinalizer(getSvc(t, cl)) || len(rt.createCalls) != 0 {
			t.Fatalf("foreign class must be ignored")
		}
	})

	t.Run("skip annotation ignored", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		svc := lbService("ns", "web", func(s *corev1.Service) {
			s.Annotations = map[string]string{AnnotationSkip: "true"}
		})
		_, _, cl := doReconcile(t, []client.Object{svc}, rt)
		if HasFinalizer(getSvc(t, cl)) || len(rt.createCalls) != 0 {
			t.Fatalf("skip annotation must be honored")
		}
	})
}

func TestReconcile_UpsertFlow(t *testing.T) {
	t.Run("no endpoints adds finalizer and requeues without router call", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		res, _, cl := doReconcile(t, []client.Object{lbService("ns", "web", nil)}, rt)
		if res.RequeueAfter != 5*time.Second {
			t.Fatalf("expected 5s requeue, got %+v", res)
		}
		if !HasFinalizer(getSvc(t, cl)) {
			t.Fatalf("expected finalizer to be added")
		}
		if len(rt.createCalls) != 0 {
			t.Fatalf("must not call router with zero endpoints")
		}
	})

	t.Run("ready endpoints create LB and patch status", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		objs := []client.Object{
			lbService("ns", "web", nil),
			endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1", "10.0.0.2"),
		}
		res, _, cl := doReconcile(t, objs, rt)
		if len(rt.createCalls) != 1 || rt.createCalls[0] != "ns/web" {
			t.Fatalf("expected one create for ns/web, got %+v", rt.createCalls)
		}
		if res.RequeueAfter != targetConvergenceDelay {
			t.Fatalf("expected convergence requeue, got %+v", res)
		}
		got := getSvc(t, cl)
		if len(got.Status.LoadBalancer.Ingress) != 1 || got.Status.LoadBalancer.Ingress[0].IP != "10.44.0.50" {
			t.Fatalf("status IP not patched: %+v", got.Status.LoadBalancer.Ingress)
		}
	})
}

func TestReconcile_DeleteFlow(t *testing.T) {
	rt := newStubRouter("10.44.0.50")
	now := metav1.Now()
	svc := lbService("ns", "web", func(s *corev1.Service) {
		s.Finalizers = []string{Finalizer}
		s.DeletionTimestamp = &now
	})
	rt.entries["ns/web"] = &router.ServiceEntry{ServiceKey: "ns/web", AssignedIP: "10.44.0.50"}
	_, _, cl := doReconcile(t, []client.Object{svc}, rt)
	if len(rt.deleteCalls) != 1 {
		t.Fatalf("expected router delete, got %+v", rt.deleteCalls)
	}
	// With the finalizer removed, the terminating object is gone.
	if err := cl.Get(testCtx(), types.NamespacedName{Namespace: "ns", Name: "web"}, &corev1.Service{}); err == nil {
		t.Fatalf("expected terminating service to be deleted after finalizer removal")
	}
}

func TestAllowedGeosAnnotation(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"missing", "", nil},
		{"single", "US", []string{"US"}},
		{"trims, uppercases and dedupes", " us , ca,US ", []string{"US", "CA"}},
		{"drops invalid entries", "US,USA,,1X", []string{"US"}},
		{"all invalid", "USA,nope", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &corev1.Service{}
			if tc.raw != "" {
				svc.Annotations = map[string]string{AnnotationAllowedGeos: tc.raw}
			}
			got := AllowedGeos(svc)
			if len(got) != len(tc.want) {
				t.Fatalf("AllowedGeos(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("AllowedGeos(%q) = %v, want %v", tc.raw, got, tc.want)
				}
			}
		})
	}

	if got := AllowedGeos(nil); got != nil {
		t.Fatalf("nil service = %v", got)
	}
}

func TestGeosChanged(t *testing.T) {
	if geosChanged(nil, nil) {
		t.Fatal("nil vs nil must be unchanged")
	}
	if geosChanged([]string{"US", "CA"}, []string{"CA", "US"}) {
		t.Fatal("order-only difference must be unchanged")
	}
	if !geosChanged([]string{"US"}, []string{"US", "CA"}) {
		t.Fatal("added country must be a change")
	}
	if !geosChanged([]string{"US", "CA"}, []string{"US"}) {
		t.Fatal("removed country must be a change")
	}
	if !geosChanged(nil, []string{"US"}) {
		t.Fatal("nil to non-empty must be a change")
	}
	if !geosChanged([]string{"US"}, nil) {
		t.Fatal("non-empty to nil must be a change")
	}
}

func TestReconcile_ForwardsGeos(t *testing.T) {
	rt := newStubRouter("10.44.0.50")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		lbService("ns", "web", func(s *corev1.Service) {
			s.Annotations = map[string]string{AnnotationAllowedGeos: "us, ca"}
		}),
		endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
	).WithStatusSubresource(&corev1.Service{}).Build()
	r := &ServiceReconciler{Client: cl, RouterClient: rt}

	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(testCtx(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	reconcile()
	if len(rt.createCalls) != 1 {
		t.Fatalf("create calls = %v", rt.createCalls)
	}
	if len(rt.lastCreateGeos) != 2 || rt.lastCreateGeos[0] != "US" || rt.lastCreateGeos[1] != "CA" {
		t.Fatalf("create geos = %v", rt.lastCreateGeos)
	}

	// Changing only the annotation must trigger an update carrying the new set.
	svc := getSvc(t, cl)
	svc.Annotations[AnnotationAllowedGeos] = "EU"
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	reconcile()
	if len(rt.updateCalls) != 1 {
		t.Fatalf("update calls = %v", rt.updateCalls)
	}
	if len(rt.lastUpdateGeos) != 1 || rt.lastUpdateGeos[0] != "EU" {
		t.Fatalf("update geos = %v", rt.lastUpdateGeos)
	}

	// An annotation that normalises to the same set must not churn the router.
	svc = getSvc(t, cl)
	svc.Annotations[AnnotationAllowedGeos] = "eu"
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	reconcile()
	if len(rt.updateCalls) != 1 {
		t.Fatalf("no-op annotation triggered another update: %v", rt.updateCalls)
	}
}
