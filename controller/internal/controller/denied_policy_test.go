package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vranyes/homelab-ingress/controller/internal/router"
)

func TestDeniedCIDRs(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{"missing", "", nil, false},
		{"single", "203.0.113.0/24", []string{"203.0.113.0/24"}, false},
		{"trims canonicalizes and dedupes", " 203.0.113.1/24 ,10.0.0.0/8,203.0.113.0/24 ", []string{"203.0.113.0/24", "10.0.0.0/8"}, false},
		{"overlapping kept", "10.0.0.0/8,10.1.0.0/16", []string{"10.0.0.0/8", "10.1.0.0/16"}, false},
		{"invalid rejected", "203.0.113.0/24,not-a-cidr", nil, true},
		{"ipv6 rejected", "2001:db8::/32", nil, true},
		{"mixed v4 and v6 rejected", "10.0.0.0/8,2001:db8::/32", nil, true},
		{"empty entries ignored", "10.0.0.0/8,,", []string{"10.0.0.0/8"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &corev1.Service{}
			if tc.raw != "" {
				svc.Annotations = map[string]string{AnnotationDeniedCIDRs: tc.raw}
			}
			got, err := DeniedCIDRs(svc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DeniedCIDRs(%q) = %v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeniedCIDRs(%q) error = %v", tc.raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("DeniedCIDRs(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("DeniedCIDRs(%q) = %v, want %v", tc.raw, got, tc.want)
				}
			}
		})
	}

	if got, err := DeniedCIDRs(nil); err != nil || got != nil {
		t.Fatalf("nil service = %v, %v", got, err)
	}
}

func TestLBPolicyFromAnnotations(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		set     bool
		want    string
		wantErr bool
	}{
		{"missing defaults to round-robin", "", false, LBPolicyRoundRobin, false},
		{"empty defaults to round-robin", "", true, LBPolicyRoundRobin, false},
		{"round-robin", "round-robin", true, "round-robin", false},
		{"hash-source-ip", "hash-source-ip", true, "hash-source-ip", false},
		{"random", "random", true, "random", false},
		{"trims and lowercases", "  Hash-Source-IP ", true, "hash-source-ip", false},
		{"least-conn rejected", "least-conn", true, "", true},
		{"blank rejected", "round robin", true, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &corev1.Service{}
			if tc.set {
				svc.Annotations = map[string]string{AnnotationLBPolicy: tc.raw}
			}
			got, err := LBPolicyFromAnnotations(svc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LBPolicyFromAnnotations(%q) = %q, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("LBPolicyFromAnnotations(%q) error = %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("LBPolicyFromAnnotations(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}

	if got, err := LBPolicyFromAnnotations(nil); err != nil || got != LBPolicyRoundRobin {
		t.Fatalf("nil service = %q, %v", got, err)
	}
}

func TestDeniedCidrsChanged(t *testing.T) {
	if deniedCidrsChanged(nil, nil) {
		t.Fatal("nil vs nil must be unchanged")
	}
	if deniedCidrsChanged([]string{"203.0.113.0/24"}, []string{"203.0.113.0/24"}) {
		t.Fatal("identical must be unchanged")
	}
	if deniedCidrsChanged([]string{"10.0.0.0/8", "203.0.113.0/24"}, []string{"203.0.113.0/24", "10.0.0.0/8"}) {
		t.Fatal("order-only difference must be unchanged")
	}
	if !deniedCidrsChanged([]string{"203.0.113.0/24"}, []string{"203.0.113.0/24", "10.0.0.0/8"}) {
		t.Fatal("added CIDR must be a change")
	}
	if !deniedCidrsChanged([]string{"10.0.0.0/8"}, nil) {
		t.Fatal("non-empty to nil must be a change")
	}
	if !deniedCidrsChanged(nil, []string{"10.0.0.0/8"}) {
		t.Fatal("nil to non-empty must be a change")
	}
}

func TestLBPolicyChanged(t *testing.T) {
	if lbPolicyChanged("", "round-robin") {
		t.Fatal("empty (default) vs round-robin must be unchanged")
	}
	if lbPolicyChanged("round-robin", "") {
		t.Fatal("round-robin vs empty (default) must be unchanged")
	}
	if lbPolicyChanged("random", "random") {
		t.Fatal("identical must be unchanged")
	}
	if !lbPolicyChanged("round-robin", "random") {
		t.Fatal("policy switch must be a change")
	}
	if !lbPolicyChanged("random", "hash-source-ip") {
		t.Fatal("policy switch must be a change")
	}
	if !lbPolicyChanged("", "random") {
		t.Fatal("default to random must be a change")
	}
}

func TestReconcile_ForwardsDeniedAndPolicy(t *testing.T) {
	rt := newStubRouter("10.44.0.50")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		lbService("ns", "web", func(s *corev1.Service) {
			s.Annotations = map[string]string{
				AnnotationDeniedCIDRs: "203.0.113.1/24, 10.0.0.0/8",
				AnnotationLBPolicy:    "Hash-Source-IP",
			}
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
	if len(rt.lastCreateDenied) != 2 || rt.lastCreateDenied[0] != "203.0.113.0/24" || rt.lastCreateDenied[1] != "10.0.0.0/8" {
		t.Fatalf("create denied = %v", rt.lastCreateDenied)
	}
	if rt.lastCreatePolicy != "hash-source-ip" {
		t.Fatalf("create policy = %q", rt.lastCreatePolicy)
	}

	// Changing only the policy must trigger an update carrying the new policy.
	svc := getSvc(t, cl)
	svc.Annotations[AnnotationLBPolicy] = "random"
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	reconcile()
	if len(rt.updateCalls) != 1 {
		t.Fatalf("update calls = %v", rt.updateCalls)
	}
	if rt.lastUpdatePolicy != "random" {
		t.Fatalf("update policy = %q", rt.lastUpdatePolicy)
	}
	// Denied CIDRs ride along unchanged.
	if len(rt.lastUpdateDenied) != 2 {
		t.Fatalf("update denied = %v", rt.lastUpdateDenied)
	}

	// An annotation that normalises to the same set must not churn the router.
	svc = getSvc(t, cl)
	svc.Annotations[AnnotationLBPolicy] = " Random "
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	reconcile()
	if len(rt.updateCalls) != 1 {
		t.Fatalf("no-op annotation triggered another update: %v", rt.updateCalls)
	}
}

func TestReconcile_InvalidDeniedKeepsPrevious(t *testing.T) {
	t.Run("create with invalid proceeds with nil and still patches status", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
			lbService("ns", "web", func(s *corev1.Service) {
				s.Annotations = map[string]string{AnnotationDeniedCIDRs: "not-a-cidr"}
			}),
			endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
		).WithStatusSubresource(&corev1.Service{}).Build()
		r := &ServiceReconciler{Client: cl, RouterClient: rt}

		if _, err := r.Reconcile(testCtx(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"},
		}); err != nil {
			t.Fatalf("reconcile with invalid denied CIDRs must not fail: %v", err)
		}
		if len(rt.createCalls) != 1 {
			t.Fatalf("expected one create with invalid denied CIDRs, got %+v", rt.createCalls)
		}
		if len(rt.lastCreateDenied) != 0 {
			t.Fatalf("create with invalid denied CIDRs must use nil: %v", rt.lastCreateDenied)
		}
		got := getSvc(t, cl)
		if len(got.Status.LoadBalancer.Ingress) != 1 || got.Status.LoadBalancer.Ingress[0].IP != "10.44.0.50" {
			t.Fatalf("status IP not patched: %+v", got.Status.LoadBalancer.Ingress)
		}
	})

	t.Run("update with invalid keeps previous and still reconciles other fields", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
			lbService("ns", "web", func(s *corev1.Service) {
				s.Annotations = map[string]string{AnnotationDeniedCIDRs: "203.0.113.0/24"}
			}),
			endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
		).WithStatusSubresource(&corev1.Service{}).Build()
		r := &ServiceReconciler{Client: cl, RouterClient: rt}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"}}

		if _, err := r.Reconcile(testCtx(), req); err != nil {
			t.Fatalf("initial reconcile: %v", err)
		}

		// Flip to an invalid value: no error, no router churn, entry keeps
		// the last-good denied CIDRs.
		svc := getSvc(t, cl)
		svc.Annotations[AnnotationDeniedCIDRs] = "bogus"
		if err := cl.Update(testCtx(), svc); err != nil {
			t.Fatalf("update annotation: %v", err)
		}
		if _, err := r.Reconcile(testCtx(), req); err != nil {
			t.Fatalf("reconcile with invalid denied CIDRs must not fail: %v", err)
		}
		if len(rt.updateCalls) != 0 {
			t.Fatalf("invalid denied CIDRs changed the router: %v", rt.updateCalls)
		}
		entry, err := rt.Get(testCtx(), "ns/web")
		if err != nil {
			t.Fatalf("get router entry: %v", err)
		}
		if len(entry.DeniedCIDRs) != 1 || entry.DeniedCIDRs[0] != "203.0.113.0/24" {
			t.Fatalf("previous denied CIDRs were not preserved: %+v", entry.DeniedCIDRs)
		}

		// Other fields still reconcile while denied CIDRs stay preserved.
		svc = getSvc(t, cl)
		svc.Annotations[AnnotationLBPolicy] = "random"
		if err := cl.Update(testCtx(), svc); err != nil {
			t.Fatalf("update policy annotation: %v", err)
		}
		if _, err := r.Reconcile(testCtx(), req); err != nil {
			t.Fatalf("reconcile policy with invalid denied CIDRs: %v", err)
		}
		if len(rt.updateCalls) != 1 {
			t.Fatalf("policy change did not update the router: %v", rt.updateCalls)
		}
		if len(rt.lastUpdateDenied) != 1 || rt.lastUpdateDenied[0] != "203.0.113.0/24" {
			t.Fatalf("update did not preserve denied CIDRs: %v", rt.lastUpdateDenied)
		}
		if rt.lastUpdatePolicy != "random" {
			t.Fatalf("update policy = %q", rt.lastUpdatePolicy)
		}
	})
}

func TestReconcile_InvalidPolicyKeepsPrevious(t *testing.T) {
	rt := newStubRouter("10.44.0.50")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		lbService("ns", "web", func(s *corev1.Service) {
			s.Annotations = map[string]string{AnnotationLBPolicy: "random"}
		}),
		endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
	).WithStatusSubresource(&corev1.Service{}).Build()
	r := &ServiceReconciler{Client: cl, RouterClient: rt}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"}}

	if _, err := r.Reconcile(testCtx(), req); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	if rt.lastCreatePolicy != "random" {
		t.Fatalf("create policy = %q", rt.lastCreatePolicy)
	}

	// An unknown policy must not reach the router and must not churn it.
	svc := getSvc(t, cl)
	svc.Annotations[AnnotationLBPolicy] = "least-conn"
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	if _, err := r.Reconcile(testCtx(), req); err != nil {
		t.Fatalf("reconcile with invalid policy must not fail: %v", err)
	}
	if len(rt.updateCalls) != 0 {
		t.Fatalf("invalid policy changed the router: %v", rt.updateCalls)
	}
	entry, err := rt.Get(testCtx(), "ns/web")
	if err != nil {
		t.Fatalf("get router entry: %v", err)
	}
	if entry.LBPolicy != "random" {
		t.Fatalf("previous policy was not preserved: %q", entry.LBPolicy)
	}

	// A subsequent valid change still reconciles.
	svc = getSvc(t, cl)
	svc.Annotations[AnnotationLBPolicy] = "hash-source-ip"
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	if _, err := r.Reconcile(testCtx(), req); err != nil {
		t.Fatalf("reconcile valid policy: %v", err)
	}
	if len(rt.updateCalls) != 1 || rt.lastUpdatePolicy != "hash-source-ip" {
		t.Fatalf("valid policy change did not update the router: %v", rt.updateCalls)
	}
}

func TestReconcile_LegacyEmptyPolicyDoesNotChurn(t *testing.T) {
	// Services created before lb-policy existed report an empty policy; the
	// default round-robin annotation must not trigger an update.
	rt := newStubRouter("10.44.0.50")
	rt.entries["ns/web"] = &router.ServiceEntry{
		ServiceKey: "ns/web",
		AssignedIP: "10.44.0.50",
		Ports: []router.PortMapping{{
			Protocol:     "tcp",
			ExternalPort: 80,
			Destinations: []router.Destination{{Addr: "10.0.0.1", Port: 8080}},
		}},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		lbService("ns", "web", nil),
		endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
	).WithStatusSubresource(&corev1.Service{}).Build()
	r := &ServiceReconciler{Client: cl, RouterClient: rt}

	if _, err := r.Reconcile(testCtx(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rt.updateCalls) != 0 {
		t.Fatalf("legacy empty policy churned the router: %v", rt.updateCalls)
	}
}
