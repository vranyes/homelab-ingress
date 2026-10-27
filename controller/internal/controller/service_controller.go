package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vranyes/homelab-ingress/controller/internal/router"
)

// ConditionHomelabIngressReady reports whether the Service has been
// provisioned on the router (True/Provisioned with an assigned IP, or
// False with a Pending/RouterUnavailable reason while converging).
const ConditionHomelabIngressReady = "HomelabIngressReady"

// Condition reasons for HomelabIngressReady.
const (
	ReasonProvisioned       = "Provisioned"
	ReasonPending           = "Pending"
	ReasonRouterUnavailable = "RouterUnavailable"
)

// Event reasons emitted on the Service.
const (
	EventReasonInvalidGeos         = "InvalidAllowedGeos"
	EventReasonInvalidAllowedCIDRs = "InvalidAllowedCIDRs"
	EventReasonInvalidDeniedCIDRs  = "InvalidDeniedCIDRs"
	EventReasonInvalidRateLimit    = "InvalidRateLimit"
	EventReasonInvalidLBPolicy     = "InvalidLBPolicy"
	EventReasonRouterUnavailable   = "RouterUnavailable"
	EventReasonGeoDisabled         = "GeoBlockingUnavailable"
)

type ServiceReconciler struct {
	client.Client
	RouterClient router.Client
	// Recorder emits Warning events on the Service for invalid annotations
	// and router outages. Nil-safe: reconcilers built without a manager
	// (unit tests) simply skip event emission.
	Recorder record.EventRecorder
}

// targetConvergenceDelay is the requeue interval after a successful
// Create/Update that grew the LB's target set. The follow-up reconcile
// converges once the router mirrors the desired target set, after which
// the no-requeue path returns and no further churn is generated. Kept on
// the order of the controller's IP-poller cadence (--poll-interval 5s) so
// that an entire pod-startup wave settles within a small number of ticks.
const targetConvergenceDelay = 5 * time.Second

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get service: %w", err)
	}

	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return ctrl.Result{}, nil
	}

	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
		return ctrl.Result{}, nil
	}

	if svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass != "homelab-ingress" {
		return ctrl.Result{}, nil
	}

	if ShouldSkip(&svc) {
		logger.Info("skipping service with skip annotation")
		return ctrl.Result{}, nil
	}

	if !svc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &svc)
	}

	if !HasFinalizer(&svc) {
		AddFinalizer(&svc)
		if err := r.Update(ctx, &svc); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
	}

	return r.reconcileUpsert(ctx, &svc)
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("homelab-ingress-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}, builder.WithPredicates(
			matchesLoadBalancerClass,
		)).
		Watches(
			&discoveryv1.EndpointSlice{},
			handler.EnqueueRequestsFromMapFunc(r.findServiceForEndpointSlice),
		).
		Complete(r)
}

// matchesLoadBalancerClass filters Service events to only those with
// type=LoadBalancer and loadBalancerClass=homelab-ingress. Because this predicate
// is applied via builder.WithPredicates it only scopes the primary For watch
// and does not block secondary Watches (like EndpointSlices).
var matchesLoadBalancerClass = predicate.Funcs{
	CreateFunc:  func(e event.CreateEvent) bool { return svcMatches(e.Object) },
	UpdateFunc:  func(e event.UpdateEvent) bool { return svcMatches(e.ObjectOld) || svcMatches(e.ObjectNew) },
	DeleteFunc:  func(e event.DeleteEvent) bool { return svcMatches(e.Object) },
	GenericFunc: func(e event.GenericEvent) bool { return svcMatches(e.Object) },
}

func svcMatches(obj client.Object) bool {
	svc, ok := obj.(*corev1.Service)
	if !ok || svc == nil {
		return false
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return false
	}
	if svc.Spec.LoadBalancerClass == nil {
		return false
	}
	return *svc.Spec.LoadBalancerClass == "homelab-ingress"
}

func (r *ServiceReconciler) findServiceForEndpointSlice(ctx context.Context, obj client.Object) []reconcile.Request {
	endpointSlice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil
	}

	// Look for the standard label linking it to the service
	svcName, exists := endpointSlice.Labels["kubernetes.io/service-name"]
	if !exists {
		return nil
	}

	// Return a request for the Service in the same namespace
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      svcName,
				Namespace: endpointSlice.Namespace,
			},
		},
	}
}

type target struct {
	Name string
	IP   string
	Port uint16
}

func (r *ServiceReconciler) reconcileUpsert(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	serviceKey := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}.String()

	var targets []target
	targets, err := r.endpointInfoForService(ctx, svc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list endpoints: %w", err)
	}
	if len(targets) == 0 {
		logger.Info("no ready endpoint IPs yet, requeueing")
		if err := r.markNotReady(ctx, svc, ReasonPending, "waiting for ready endpoints"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	ports := buildPortMappings(svc, targets)
	// If any mapping couldn't resolve a named target port (Port == 0), requeue
	for _, m := range ports {
		for _, d := range m.Destinations {
			if d.Port == 0 {
				logger.Info("no target port resolved for named port yet, requeueing")
				if err := r.markNotReady(ctx, svc, ReasonPending, "waiting for named target port to resolve"); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		}
	}

	geos := AllowedGeos(svc)
	// A fully-invalid geos annotation normalises to nil (allow all). Surface
	// it so a typo doesn't silently open the service.
	if rawGeos, ok := svc.Annotations[AnnotationAllowedGeos]; ok && strings.TrimSpace(rawGeos) != "" && len(geos) == 0 {
		logger.Info("invalid allowed-geos annotation, allowing all traffic",
			"service", serviceKey, "annotation", rawGeos)
		r.warn(svc, EventReasonInvalidGeos,
			fmt.Sprintf("invalid %s %q: allowing all traffic", AnnotationAllowedGeos, rawGeos))
	}
	cidrs, cidrErr := AllowedCIDRs(svc)
	if cidrErr != nil {
		// Like the rate-limit annotation: log and keep the last known good
		// CIDRs instead of failing the reconcile (which would hot-loop).
		// Data plane keeps serving; status still patches.
		logger.Info("invalid allowed-cidrs annotation, keeping previous CIDRs",
			"service", serviceKey, "error", cidrErr)
		r.warn(svc, EventReasonInvalidAllowedCIDRs,
			fmt.Sprintf("invalid %s, keeping previous CIDRs: %v", AnnotationAllowedCIDRs, cidrErr))
	}
	desiredRateLimit, rateLimitErr := RateLimitFromAnnotations(svc)
	if rateLimitErr != nil {
		// A malformed annotation must not silently disable protection. Keep
		// the limit the router already has; the rest of the spec still
		// reconciles.
		logger.Info("invalid rate-limit annotation, keeping the previous limit",
			"service", serviceKey, "error", rateLimitErr)
		r.warn(svc, EventReasonInvalidRateLimit,
			fmt.Sprintf("invalid %s, keeping previous limit: %v", AnnotationRateLimit, rateLimitErr))
	}
	denied, deniedErr := DeniedCIDRs(svc)
	if deniedErr != nil {
		// Like the allowed-cidrs annotation: log and keep the last known
		// good CIDRs instead of failing the reconcile (which would hot-loop).
		logger.Info("invalid denied-cidrs annotation, keeping previous CIDRs",
			"service", serviceKey, "error", deniedErr)
		r.warn(svc, EventReasonInvalidDeniedCIDRs,
			fmt.Sprintf("invalid %s, keeping previous CIDRs: %v", AnnotationDeniedCIDRs, deniedErr))
	}
	desiredPolicy, policyErr := LBPolicyFromAnnotations(svc)
	if policyErr != nil {
		// Like the rate-limit annotation: log and keep the router's previous
		// policy instead of silently changing load balancing.
		logger.Info("invalid lb-policy annotation, keeping the previous policy",
			"service", serviceKey, "error", policyErr)
		r.warn(svc, EventReasonInvalidLBPolicy,
			fmt.Sprintf("invalid %s, keeping previous policy: %v", AnnotationLBPolicy, policyErr))
	}

	existing, err := r.RouterClient.Get(ctx, serviceKey)
	if err != nil {
		if router.IsErrNotFound(err) {
			// fallthrough to create
		} else if router.IsRetryable(err) {
			logger.Info("router temporarily unavailable, requeueing", "service", serviceKey, "error", err)
			r.warn(svc, EventReasonRouterUnavailable,
				fmt.Sprintf("router temporarily unavailable: %v", err))
			if err := r.markNotReady(ctx, svc, ReasonRouterUnavailable, "router temporarily unavailable"); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		} else {
			return ctrl.Result{}, fmt.Errorf("get from router: %w", err)
		}
	}

	if existing == nil {
		// Nothing to preserve on create; an invalid annotation simply means
		// the first install has no limit / no CIDRs / the default policy.
		createCIDRs := cidrs
		if cidrErr != nil {
			createCIDRs = nil
		}
		createDenied := denied
		if deniedErr != nil {
			createDenied = nil
		}
		if policyErr != nil {
			desiredPolicy = LBPolicyRoundRobin
		}
		createRateLimit := toRouterRateLimit(desiredRateLimit)
		logger.Info("creating load balancer on router", "service", serviceKey)
		entry, err := r.RouterClient.Create(ctx, serviceKey, ports, geos, createCIDRs, createDenied, desiredPolicy, createRateLimit)
		if err != nil {
			if router.IsRetryable(err) {
				logger.Info("router temporarily unavailable during create, requeueing", "service", serviceKey, "error", err)
				r.warn(svc, EventReasonRouterUnavailable,
					fmt.Sprintf("router temporarily unavailable during create: %v", err))
				if err := r.markNotReady(ctx, svc, ReasonRouterUnavailable, "router temporarily unavailable"); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			if isGeoBlockingError(err) {
				r.warn(svc, EventReasonGeoDisabled,
					"service requests allowed-geos but geo-blocking is not configured on the router")
			}
			return ctrl.Result{}, fmt.Errorf("create on router: %w", err)
		}
		if entry.AssignedIP == "" {
			logger.Info("router has not assigned IP yet, requeueing", "service", serviceKey)
			if err := r.markNotReady(ctx, svc, ReasonPending, "waiting for router to assign IP"); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// Reschedule a single follow-up reconcile: the controller fetched the
		// first ready endpoint slice during this pass, but other pods may not
		// be ready yet. Without this requeue the Service would stay glued to
		// whatever partial target set is in the router until the next
		// EndpointSlice watch event fires (which can take >60s in steady state).
		// The follow-up reconcile will short-circuit when the router already
		// matches the desired target set.
		if _, err := r.patchStatus(ctx, svc, entry.AssignedIP); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: targetConvergenceDelay}, nil
	}

	if cidrErr != nil {
		// Preserve the last known good CIDRs while still reconciling ports
		// and geo rules.
		cidrs = existing.AllowedCIDRs
	}
	if rateLimitErr != nil {
		// Preserve the last known good value while still reconciling ports
		// and geo rules.
		desiredRateLimit = fromRouterRateLimit(existing.RateLimit)
	}
	if deniedErr != nil {
		// Preserve the last known good denied CIDRs.
		denied = existing.DeniedCIDRs
	}
	if policyErr != nil {
		// Preserve the last known good policy while still reconciling ports
		// and geo rules.
		desiredPolicy = existing.LBPolicy
	}

	if portsChanged(existing.Ports, ports) ||
		geosChanged(existing.AllowedGeos, geos) ||
		cidrsChanged(existing.AllowedCIDRs, cidrs) ||
		deniedCidrsChanged(existing.DeniedCIDRs, denied) ||
		lbPolicyChanged(existing.LBPolicy, desiredPolicy) ||
		rateLimitChanged(fromRouterRateLimit(existing.RateLimit), desiredRateLimit) {
		logger.Info("updating load balancer on router", "service", serviceKey)
		if err := r.RouterClient.Update(ctx, serviceKey, ports, geos, cidrs, denied, desiredPolicy, toRouterRateLimit(desiredRateLimit)); err != nil {
			if router.IsRetryable(err) {
				logger.Info("router temporarily unavailable during update, requeueing", "service", serviceKey, "error", err)
				r.warn(svc, EventReasonRouterUnavailable,
					fmt.Sprintf("router temporarily unavailable during update: %v", err))
				if err := r.markNotReady(ctx, svc, ReasonRouterUnavailable, "router temporarily unavailable"); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			if isGeoBlockingError(err) {
				r.warn(svc, EventReasonGeoDisabled,
					"service requests allowed-geos but geo-blocking is not configured on the router")
			}
			return ctrl.Result{}, fmt.Errorf("update on router: %w", err)
		}
		// Same convergence requeue as the Create branch: a single follow-up
		// pass converges once all backing pods are ready. routesEqual() flips
		// to false once steady state is reached, suppressing further requeues.
		if existing.AssignedIP != "" {
			if _, err := r.patchStatus(ctx, svc, existing.AssignedIP); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: targetConvergenceDelay}, nil
		}
	}

	if existing.AssignedIP == "" {
		logger.Info("router has not assigned IP yet, requeueing", "service", serviceKey)
		if err := r.markNotReady(ctx, svc, ReasonPending, "waiting for router to assign IP"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	return r.patchStatus(ctx, svc, existing.AssignedIP)
}

func (r *ServiceReconciler) reconcileDelete(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	serviceKey := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}.String()

	logger.Info("deleting load balancer from router", "service", serviceKey)
	if err := r.RouterClient.Delete(ctx, serviceKey); err != nil {
		if router.IsRetryable(err) {
			logger.Info("router temporarily unavailable during delete, requeueing", "service", serviceKey, "error", err)
			r.warn(svc, EventReasonRouterUnavailable,
				fmt.Sprintf("router temporarily unavailable during delete: %v", err))
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("delete from router: %w", err)
	}

	RemoveFinalizer(svc)
	if err := r.Update(ctx, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

// endpointInfoForService returns the ready endpoint IPs for a Service and a
// mapping of named ports to numeric ports discovered from EndpointSlices in the
// same namespace for that Service. The namedPorts map is keyed by port name
// and contains the numeric port that endpoints advertise for that name. This
// allows resolving Service ports that target a named port rather than a
// numeric TargetPort.
func (r *ServiceReconciler) endpointInfoForService(ctx context.Context, svc *corev1.Service) ([]target, error) {
	// Select EndpointSlices for this Service by the standard label.
	sel := labels.SelectorFromSet(labels.Set{"kubernetes.io/service-name": svc.Name})
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices, &client.ListOptions{
		Namespace:     svc.Namespace,
		LabelSelector: sel,
	}); err != nil {
		return nil, err
	}

	var targets []target
	logger := log.FromContext(ctx)
	logger.V(1).Info("found endpoint slices for service", "service", types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, "count", len(slices.Items))
	for _, slice := range slices.Items {
		var ips []string
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				if addr == "" {
					continue
				}
				ips = append(ips, addr)
			}
		}
		logger.V(1).Info("found ready endpoints in slice", "slice", slice.Name, "readyIPs", ips)
		// Record ports advertised by this slice. EndpointSlice.Ports may contain
		// a name and port number. We'll prefer the first numeric value seen for
		// each name.
		for _, sp := range slice.Ports {
			if sp.Name == nil || sp.Port == nil {
				continue
			}
			for _, ip := range ips {
				targets = append(targets, target{
					Name: *sp.Name,
					Port: uint16(*sp.Port),
					IP:   ip,
				})
			}
		}
		logger.V(1).Info("added targets from slice", "slice", slice.Name, "targets", targets)
	}

	return targets, nil
}

// buildPortMappings constructs router.PortMapping entries for the Service
// ports. If a ServicePort.TargetPort is a name, the function consults the
// namedPorts map (from EndpointSlices) to resolve the numeric pod port. If a
// named port is not found, Port will be 0 to indicate unresolved.
func buildPortMappings(svc *corev1.Service, targets []target) []router.PortMapping {
	var mappings []router.PortMapping
	for _, p := range svc.Spec.Ports {
		proto := "tcp"
		if p.Protocol == corev1.ProtocolUDP {
			proto = "udp"
		}

		var podPort uint16
		if p.TargetPort.Type == intstr.Int {
			podPort = uint16(p.TargetPort.IntValue())
		} else if p.TargetPort.Type == intstr.String {
			name := p.TargetPort.StrVal
			if name != "" {
				for _, t := range targets {
					if t.Name == name {
						podPort = t.Port
						break
					}
				}
			}
		}

		var podIPs []string
		for _, t := range targets {
			if t.Name == p.Name {
				podIPs = append(podIPs, t.IP)
			}
		}

		dst := make([]router.Destination, len(podIPs))
		for j, ip := range podIPs {
			dst[j] = router.Destination{Addr: ip, Port: podPort}
		}

		mappings = append(mappings, router.PortMapping{
			Protocol:     proto,
			ExternalPort: uint16(p.Port),
			Destinations: dst,
		})
	}
	return mappings
}

// warn emits a Warning event on the Service. It is a no-op when the
// reconciler has no Recorder (e.g. unit tests built without a manager).
func (r *ServiceReconciler) warn(svc *corev1.Service, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(svc, corev1.EventTypeWarning, reason, message)
}

// isGeoBlockingError reports whether err is the daemon's rejection of a
// request that carries allowed-geos while no country database is configured
// (ADR 0003). The controller cannot know the daemon's geo state upfront, so
// it surfaces the rejection as an event instead.
func isGeoBlockingError(err error) bool {
	if err == nil {
		return false
	}
	if status.Code(err) != codes.InvalidArgument {
		return false
	}
	return strings.Contains(err.Error(), "geo-blocking")
}

// markNotReady records a False HomelabIngressReady condition (leaving the
// ingress IP untouched) and reports a patch failure, if any.
func (r *ServiceReconciler) markNotReady(ctx context.Context, svc *corev1.Service, reason, message string) error {
	cond := metav1.Condition{
		Type:               ConditionHomelabIngressReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: svc.Generation,
	}
	_, err := r.patchStatusWithCondition(ctx, svc, currentIngressIP(svc), &cond)
	return err
}

func currentIngressIP(svc *corev1.Service) string {
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		return svc.Status.LoadBalancer.Ingress[0].IP
	}
	return ""
}

func conditionNeedsUpdate(svc *corev1.Service, cond *metav1.Condition) bool {
	if cond == nil {
		return false
	}
	existing := meta.FindStatusCondition(svc.Status.Conditions, cond.Type)
	if existing == nil {
		return true
	}
	return existing.Status != cond.Status ||
		existing.Reason != cond.Reason ||
		existing.Message != cond.Message
}

func (r *ServiceReconciler) patchStatus(ctx context.Context, svc *corev1.Service, ip string) (ctrl.Result, error) {
	if ip == "" {
		return ctrl.Result{}, nil
	}
	// An assigned IP means the data plane is programmed: the service is
	// ready. The condition travels with the ingress IP patch so both land
	// atomically; existing unrelated conditions are preserved by the
	// MergeFrom patch.
	cond := &metav1.Condition{
		Type:               ConditionHomelabIngressReady,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonProvisioned,
		Message:            "assigned IP " + ip,
		ObservedGeneration: svc.Generation,
	}
	return r.patchStatusWithCondition(ctx, svc, ip, cond)
}

func (r *ServiceReconciler) patchStatusWithCondition(ctx context.Context, svc *corev1.Service, ip string, cond *metav1.Condition) (ctrl.Result, error) {
	if currentIngressIP(svc) == ip && !conditionNeedsUpdate(svc, cond) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(svc.DeepCopy())
	if ip != "" {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
	}
	if cond != nil {
		meta.SetStatusCondition(&svc.Status.Conditions, *cond)
	}
	if err := r.Status().Patch(ctx, svc, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch service status: %w", err)
	}
	return ctrl.Result{}, nil
}

func portsChanged(existing, desired []router.PortMapping) bool {
	if len(existing) != len(desired) {
		return true
	}
	for i := range existing {
		if existing[i].Protocol != desired[i].Protocol {
			return true
		}
		if existing[i].ExternalPort != desired[i].ExternalPort {
			return true
		}
		if !destinationsEqual(existing[i].Destinations, desired[i].Destinations) {
			return true
		}
	}
	return false
}

func toRouterRateLimit(rl *RateLimit) *router.RateLimit {
	if rl == nil {
		return nil
	}
	return &router.RateLimit{Rate: rl.Rate, Burst: rl.Burst}
}

func fromRouterRateLimit(rl *router.RateLimit) *RateLimit {
	if rl == nil {
		return nil
	}
	return &RateLimit{Rate: rl.Rate, Burst: rl.Burst}
}

// cidrsChanged compares allow-lists as sets so annotation ordering does not
// churn the router. Both sides are canonical (IPNet.String()) forms.
func cidrsChanged(existing, desired []string) bool {
	if len(existing) != len(desired) {
		return true
	}
	have := make(map[string]struct{}, len(existing))
	for _, c := range existing {
		have[c] = struct{}{}
	}
	for _, c := range desired {
		if _, ok := have[c]; !ok {
			return true
		}
	}
	return false
}

// deniedCidrsChanged compares deny-lists as sets so annotation ordering does
// not churn the router. Both sides are canonical (IPNet.String()) forms.
func deniedCidrsChanged(existing, desired []string) bool {
	return cidrsChanged(existing, desired)
}

// geosChanged compares allow-lists as sets so annotation ordering does not
// churn the router.
func geosChanged(existing, desired []string) bool {
	if len(existing) != len(desired) {
		return true
	}
	have := make(map[string]struct{}, len(existing))
	for _, cc := range existing {
		have[cc] = struct{}{}
	}
	for _, cc := range desired {
		if _, ok := have[cc]; !ok {
			return true
		}
	}
	return false
}

func destinationsEqual(a, b []router.Destination) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]uint16)
	for _, d := range a {
		m[d.Addr] = d.Port
	}
	for _, d := range b {
		if m[d.Addr] != d.Port {
			return false
		}
	}
	return true
}
