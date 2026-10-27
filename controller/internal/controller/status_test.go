package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vranyes/homelab-ingress/controller/internal/router"
)

// errRouter wraps stubRouter with injectable Get/Create failures.
type errRouter struct {
	*stubRouter
	getErr    error
	createErr error
}

func (e *errRouter) Get(ctx context.Context, key string) (*router.ServiceEntry, error) {
	if e.getErr != nil {
		return nil, e.getErr
	}
	return e.stubRouter.Get(ctx, key)
}

func (e *errRouter) Create(ctx context.Context, key string, ports []router.PortMapping, geos []string, cidrs []string, denied []string, lbPolicy string, rateLimit *router.RateLimit) (*router.ServiceEntry, error) {
	if e.createErr != nil {
		return nil, e.createErr
	}
	return e.stubRouter.Create(ctx, key, ports, geos, cidrs, denied, lbPolicy, rateLimit)
}

func reconcileWithRecorder(t *testing.T, objs []client.Object, rc router.Client) (ctrl.Result, client.Client, *record.FakeRecorder) {
	t.Helper()
	rec := record.NewFakeRecorder(32)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&corev1.Service{}).Build()
	r := &ServiceReconciler{Client: cl, RouterClient: rc, Recorder: rec}
	res, err := r.Reconcile(testCtx(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res, cl, rec
}

func readyCond(t *testing.T, cl client.Client) *metav1.Condition {
	t.Helper()
	svc := getSvc(t, cl)
	cond := meta.FindStatusCondition(svc.Status.Conditions, ConditionHomelabIngressReady)
	if cond == nil {
		t.Fatalf("HomelabIngressReady condition missing: %+v", svc.Status.Conditions)
	}
	return cond
}

// drainEvents collects all queued fake-recorder events without blocking.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func hasEventWithReason(events []string, eventType, reason string) bool {
	for _, ev := range events {
		if strings.Contains(ev, eventType) && strings.Contains(ev, reason) {
			return true
		}
	}
	return false
}

func TestPatchStatusSetsReadyCondition(t *testing.T) {
	rt := newStubRouter("10.44.0.50")
	objs := []client.Object{
		lbService("ns", "web", nil),
		endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
	}
	res, cl, _ := reconcileWithRecorder(t, objs, rt)
	if res.RequeueAfter != targetConvergenceDelay {
		t.Fatalf("expected convergence requeue, got %+v", res)
	}
	cond := readyCond(t, cl)
	if cond.Status != metav1.ConditionTrue || cond.Reason != ReasonProvisioned {
		t.Fatalf("condition = %+v, want True/Provisioned", cond)
	}
	if !strings.Contains(cond.Message, "10.44.0.50") {
		t.Fatalf("condition message %q does not mention the IP", cond.Message)
	}
}

func TestPatchStatusIsIdempotent(t *testing.T) {
	rt := newStubRouter("10.44.0.50")
	objs := []client.Object{
		lbService("ns", "web", nil),
		endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&corev1.Service{}).Build()
	r := &ServiceReconciler{Client: cl, RouterClient: rt, Recorder: record.NewFakeRecorder(32)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"}}

	if _, err := r.Reconcile(testCtx(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	before := getSvc(t, cl)
	condBefore := meta.FindStatusCondition(before.Status.Conditions, ConditionHomelabIngressReady)
	if condBefore == nil {
		t.Fatalf("condition missing after first reconcile")
	}

	// Second pass: router already matches, so no update and no status write.
	res2, err := r.Reconcile(testCtx(), req)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if res2 != (ctrl.Result{}) {
		t.Fatalf("steady state should not requeue, got %+v", res2)
	}
	if len(rt.updateCalls) != 0 {
		t.Fatalf("steady state must not update the router: %v", rt.updateCalls)
	}
	after := getSvc(t, cl)
	condAfter := meta.FindStatusCondition(after.Status.Conditions, ConditionHomelabIngressReady)
	if condAfter == nil || *condAfter != *condBefore {
		t.Fatalf("condition changed on no-op reconcile: %+v -> %+v", condBefore, condAfter)
	}
}

func TestPatchStatusPreservesUnrelatedConditions(t *testing.T) {
	rt := newStubRouter("10.44.0.50")
	objs := []client.Object{
		lbService("ns", "web", nil),
		endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
	}
	_, cl, _ := reconcileWithRecorder(t, objs, rt)

	svc := getSvc(t, cl)
	patch := client.MergeFrom(svc.DeepCopy())
	meta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{
		Type: "External", Status: metav1.ConditionTrue, Reason: "Other", Message: "set by someone else",
	})
	if err := cl.Status().Patch(testCtx(), svc, patch); err != nil {
		t.Fatalf("seed external condition: %v", err)
	}

	// Trigger another status-affecting pass (annotation change forces update +
	// patchStatus). The foreign condition must survive.
	svc = getSvc(t, cl)
	svc.Annotations = map[string]string{AnnotationAllowedGeos: "US"}
	if err := cl.Update(testCtx(), svc); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	r := &ServiceReconciler{Client: cl, RouterClient: rt, Recorder: record.NewFakeRecorder(32)}
	if _, err := r.Reconcile(testCtx(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getSvc(t, cl)
	if c := meta.FindStatusCondition(got.Status.Conditions, "External"); c == nil {
		t.Fatalf("foreign condition was clobbered: %+v", got.Status.Conditions)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, ConditionHomelabIngressReady); c == nil {
		t.Fatalf("ready condition missing: %+v", got.Status.Conditions)
	}
}

func TestNoEndpointsSetsPendingCondition(t *testing.T) {
	rt := newStubRouter("10.44.0.50")
	res, cl, _ := reconcileWithRecorder(t, []client.Object{lbService("ns", "web", nil)}, rt)
	if res.RequeueAfter != 5*time.Second {
		t.Fatalf("expected 5s requeue, got %+v", res)
	}
	cond := readyCond(t, cl)
	if cond.Status != metav1.ConditionFalse || cond.Reason != ReasonPending {
		t.Fatalf("condition = %+v, want False/Pending", cond)
	}
	if len(rt.createCalls) != 0 {
		t.Fatalf("must not call router with zero endpoints")
	}
}

func TestInvalidAnnotationsEmitWarningEvents(t *testing.T) {
	t.Run("fully-invalid geos warns and allows all", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		objs := []client.Object{
			lbService("ns", "web", func(s *corev1.Service) {
				s.Annotations = map[string]string{AnnotationAllowedGeos: "USA,nope"}
			}),
			endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
		}
		_, cl, rec := reconcileWithRecorder(t, objs, rt)
		if !hasEventWithReason(drainEvents(rec), corev1.EventTypeWarning, EventReasonInvalidGeos) {
			t.Fatalf("expected Warning %s event", EventReasonInvalidGeos)
		}
		// Invalid geos still proceeds (allow-all) and patches status.
		if len(rt.createCalls) != 1 {
			t.Fatalf("create calls = %v", rt.createCalls)
		}
		if c := readyCond(t, cl); c.Status != metav1.ConditionTrue {
			t.Fatalf("condition = %+v, want True", c)
		}
	})

	t.Run("invalid allowed-cidrs warns", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		objs := []client.Object{
			lbService("ns", "web", func(s *corev1.Service) {
				s.Annotations = map[string]string{AnnotationAllowedCIDRs: "not-a-cidr"}
			}),
			endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
		}
		_, _, rec := reconcileWithRecorder(t, objs, rt)
		if !hasEventWithReason(drainEvents(rec), corev1.EventTypeWarning, EventReasonInvalidAllowedCIDRs) {
			t.Fatalf("expected Warning %s event", EventReasonInvalidAllowedCIDRs)
		}
	})

	t.Run("invalid denied-cidrs warns and keeps last-good", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		objs := []client.Object{
			lbService("ns", "web", func(s *corev1.Service) {
				s.Annotations = map[string]string{AnnotationDeniedCIDRs: "10.9.0.0/16"}
			}),
			endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
		}
		cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
			WithStatusSubresource(&corev1.Service{}).Build()
		rec := record.NewFakeRecorder(32)
		r := &ServiceReconciler{Client: cl, RouterClient: rt, Recorder: rec}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"}}
		if _, err := r.Reconcile(testCtx(), req); err != nil {
			t.Fatalf("initial reconcile: %v", err)
		}

		svc := getSvc(t, cl)
		svc.Annotations[AnnotationDeniedCIDRs] = "bogus"
		if err := cl.Update(testCtx(), svc); err != nil {
			t.Fatalf("update annotation: %v", err)
		}
		if _, err := r.Reconcile(testCtx(), req); err != nil {
			t.Fatalf("reconcile with invalid denied CIDRs must not fail: %v", err)
		}
		if !hasEventWithReason(drainEvents(rec), corev1.EventTypeWarning, EventReasonInvalidDeniedCIDRs) {
			t.Fatalf("expected Warning %s event", EventReasonInvalidDeniedCIDRs)
		}
		if len(rt.updateCalls) != 0 {
			t.Fatalf("invalid denied CIDRs changed the router: %v", rt.updateCalls)
		}
		entry, err := rt.Get(testCtx(), "ns/web")
		if err != nil {
			t.Fatalf("get router entry: %v", err)
		}
		if len(entry.DeniedCIDRs) != 1 || entry.DeniedCIDRs[0] != "10.9.0.0/16" {
			t.Fatalf("previous denied CIDRs were not preserved: %+v", entry.DeniedCIDRs)
		}
	})

	t.Run("invalid rate-limit warns", func(t *testing.T) {
		rt := newStubRouter("10.44.0.50")
		objs := []client.Object{
			lbService("ns", "web", func(s *corev1.Service) {
				s.Annotations = map[string]string{AnnotationRateLimit: "lots"}
			}),
			endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
		}
		_, _, rec := reconcileWithRecorder(t, objs, rt)
		if !hasEventWithReason(drainEvents(rec), corev1.EventTypeWarning, EventReasonInvalidRateLimit) {
			t.Fatalf("expected Warning %s event", EventReasonInvalidRateLimit)
		}
	})
}

func TestRouterRetryableEmitsWarningAndPending(t *testing.T) {
	base := newStubRouter("10.44.0.50")
	rt := &errRouter{stubRouter: base, getErr: status.Error(codes.Unavailable, "connection refused")}
	objs := []client.Object{
		lbService("ns", "web", nil),
		endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
	}
	res, cl, rec := reconcileWithRecorder(t, objs, rt)
	if res.RequeueAfter != 5*time.Second {
		t.Fatalf("expected 5s requeue, got %+v", res)
	}
	if !hasEventWithReason(drainEvents(rec), corev1.EventTypeWarning, EventReasonRouterUnavailable) {
		t.Fatalf("expected Warning %s event", EventReasonRouterUnavailable)
	}
	cond := readyCond(t, cl)
	if cond.Status != metav1.ConditionFalse || cond.Reason != ReasonRouterUnavailable {
		t.Fatalf("condition = %+v, want False/RouterUnavailable", cond)
	}
}

func TestGeoDisabledEmitsWarningEvent(t *testing.T) {
	base := newStubRouter("10.44.0.50")
	rt := &errRouter{stubRouter: base,
		createErr: status.Error(codes.InvalidArgument, "geo-blocking is not configured; no geo database is available")}
	objs := []client.Object{
		lbService("ns", "web", func(s *corev1.Service) {
			s.Annotations = map[string]string{AnnotationAllowedGeos: "US"}
		}),
		endpointSlice("ns", "web-a", "web", "http", 8080, true, "10.0.0.1"),
	}
	rec := record.NewFakeRecorder(32)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&corev1.Service{}).Build()
	r := &ServiceReconciler{Client: cl, RouterClient: rt, Recorder: rec}
	_, err := r.Reconcile(testCtx(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"},
	})
	if err == nil {
		t.Fatalf("geo-disabled create must still return an error")
	}
	if !hasEventWithReason(drainEvents(rec), corev1.EventTypeWarning, EventReasonGeoDisabled) {
		t.Fatalf("expected Warning %s event", EventReasonGeoDisabled)
	}
}
