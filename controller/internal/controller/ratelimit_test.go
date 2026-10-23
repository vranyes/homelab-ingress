package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vranyes/homelab-ingress/controller/internal/router"
)

func rateLimitSvc(rate, burst string) *corev1.Service {
	svc := &corev1.Service{}
	svc.Annotations = map[string]string{}
	if rate != "" {
		svc.Annotations[AnnotationRateLimit] = rate
	}
	if burst != "" {
		svc.Annotations[AnnotationRateLimitBurst] = burst
	}
	return svc
}

func TestRateLimitFromAnnotations(t *testing.T) {
	tests := []struct {
		name    string
		rate    string
		burst   string
		want    *RateLimit
		wantErr bool
	}{
		{name: "missing annotations", want: nil},
		{name: "rate only defaults burst to rate", rate: "5", want: &RateLimit{Rate: 5, Burst: 5}},
		{name: "trims whitespace", rate: " 5 ", want: &RateLimit{Rate: 5, Burst: 5}},
		{name: "explicit burst", rate: "5", burst: "20", want: &RateLimit{Rate: 5, Burst: 20}},
		{name: "burst smaller than rate", rate: "20", burst: "5", want: &RateLimit{Rate: 20, Burst: 5}},
		{name: "burst without rate is ignored", burst: "20", want: nil},
		{name: "zero rate invalid", rate: "0", wantErr: true},
		{name: "negative rate invalid", rate: "-1", wantErr: true},
		{name: "non-numeric rate invalid", rate: "10/s", wantErr: true},
		{name: "zero burst invalid", rate: "5", burst: "0", wantErr: true},
		{name: "non-numeric burst invalid", rate: "5", burst: "lots", wantErr: true},
		{name: "rate overflow invalid", rate: "18446744073709551616", wantErr: true},
		{name: "burst overflow invalid", rate: "5", burst: "4294967296", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RateLimitFromAnnotations(rateLimitSvc(tc.rate, tc.burst))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("nil service", func(t *testing.T) {
		if got, err := RateLimitFromAnnotations(nil); err != nil || got != nil {
			t.Fatalf("nil service: got %+v, err %v", got, err)
		}
	})
}

func TestReconcile_ForwardsRateLimit(t *testing.T) {
	rt := newStubRouter("10.44.0.50")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		lbService("ns", "web", func(s *corev1.Service) {
			s.Annotations = map[string]string{AnnotationRateLimit: "5"}
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
	wantRL := func(got *router.RateLimit, rate uint64, burst uint32) {
		t.Helper()
		if got == nil || got.Rate != rate || got.Burst != burst {
			t.Fatalf("rate limit = %+v, want rate=%d burst=%d", got, rate, burst)
		}
	}

	reconcile()
	if len(rt.createCalls) != 1 {
		t.Fatalf("create calls = %v", rt.createCalls)
	}
	wantRL(rt.lastCreateRateLimit, 5, 5)

	// Changing only the burst must update the router.
	svc := getSvc(t, cl)
	svc.Annotations[AnnotationRateLimitBurst] = "20"
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	reconcile()
	if len(rt.updateCalls) != 1 {
		t.Fatalf("update calls = %v", rt.updateCalls)
	}
	wantRL(rt.lastUpdateRateLimit, 5, 20)

	// Same normalised values ("5"/"20") must not churn, even if written
	// differently.
	svc = getSvc(t, cl)
	svc.Annotations[AnnotationRateLimit] = " 5 "
	svc.Annotations[AnnotationRateLimitBurst] = "020"
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	reconcile()
	if len(rt.updateCalls) != 1 {
		t.Fatalf("normalisation triggered an update: %v", rt.updateCalls)
	}

	// Removing the annotations clears the limit.
	svc = getSvc(t, cl)
	delete(svc.Annotations, AnnotationRateLimit)
	delete(svc.Annotations, AnnotationRateLimitBurst)
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	reconcile()
	if len(rt.updateCalls) != 2 {
		t.Fatalf("removal did not update the router: %v", rt.updateCalls)
	}
	if rt.lastUpdateRateLimit != nil {
		t.Fatalf("expected nil rate limit after removal, got %+v", rt.lastUpdateRateLimit)
	}
}

func TestReconcile_InvalidRateLimitKeepsPrevious(t *testing.T) {
	rt := newStubRouter("10.44.0.50")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		lbService("ns", "web", func(s *corev1.Service) {
			s.Annotations = map[string]string{AnnotationRateLimit: "5"}
		}),
		endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
	).WithStatusSubresource(&corev1.Service{}).Build()
	r := &ServiceReconciler{Client: cl, RouterClient: rt}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"}}

	if _, err := r.Reconcile(testCtx(), req); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	if len(rt.createCalls) != 1 {
		t.Fatalf("create calls = %v", rt.createCalls)
	}

	svc := getSvc(t, cl)
	svc.Annotations[AnnotationRateLimit] = "not-a-number"
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	if _, err := r.Reconcile(testCtx(), req); err != nil {
		t.Fatalf("reconcile with invalid annotation must not fail: %v", err)
	}
	if len(rt.updateCalls) != 0 {
		t.Fatalf("invalid annotation changed the router: %v", rt.updateCalls)
	}
	entry, err := rt.Get(testCtx(), "ns/web")
	if err != nil {
		t.Fatalf("get router entry: %v", err)
	}
	if entry.RateLimit == nil || entry.RateLimit.Rate != 5 || entry.RateLimit.Burst != 5 {
		t.Fatalf("previous limit was not preserved: %+v", entry.RateLimit)
	}
}

func TestRateLimitChanged(t *testing.T) {
	tests := []struct {
		name     string
		existing *RateLimit
		desired  *RateLimit
		want     bool
	}{
		{name: "both nil", want: false},
		{name: "nil to set", desired: &RateLimit{Rate: 5, Burst: 5}, want: true},
		{name: "set to nil", existing: &RateLimit{Rate: 5, Burst: 5}, want: true},
		{name: "equal", existing: &RateLimit{Rate: 5, Burst: 10}, desired: &RateLimit{Rate: 5, Burst: 10}, want: false},
		{name: "rate changed", existing: &RateLimit{Rate: 5, Burst: 10}, desired: &RateLimit{Rate: 6, Burst: 10}, want: true},
		{name: "burst changed", existing: &RateLimit{Rate: 5, Burst: 10}, desired: &RateLimit{Rate: 5, Burst: 11}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rateLimitChanged(tc.existing, tc.desired); got != tc.want {
				t.Fatalf("rateLimitChanged() = %v, want %v", got, tc.want)
			}
		})
	}
}
