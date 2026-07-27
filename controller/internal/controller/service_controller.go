package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/vranyes/homelab-ingress/controller/internal/router"
)

type ServiceReconciler struct {
	client.Client
	RouterClient router.Client
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
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	ports := buildPortMappings(svc, targets)
	// If any mapping couldn't resolve a named target port (Port == 0), requeue
	for _, m := range ports {
		for _, d := range m.Destinations {
			if d.Port == 0 {
				logger.Info("no target port resolved for named port yet, requeueing")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		}
	}

	existing, err := r.RouterClient.Get(ctx, serviceKey)
	if err != nil {
		if router.IsErrNotFound(err) {
			// fallthrough to create
		} else if router.IsRetryable(err) {
			logger.Info("router temporarily unavailable, requeueing", "service", serviceKey, "error", err)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		} else {
			return ctrl.Result{}, fmt.Errorf("get from router: %w", err)
		}
	}

	if existing == nil {
		logger.Info("creating load balancer on router", "service", serviceKey)
		entry, err := r.RouterClient.Create(ctx, serviceKey, ports)
		if err != nil {
			if router.IsRetryable(err) {
				logger.Info("router temporarily unavailable during create, requeueing", "service", serviceKey, "error", err)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("create on router: %w", err)
		}
		if entry.AssignedIP == "" {
			logger.Info("router has not assigned IP yet, requeueing", "service", serviceKey)
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

	if portsChanged(existing.Ports, ports) {
		logger.Info("updating load balancer on router", "service", serviceKey)
		if err := r.RouterClient.Update(ctx, serviceKey, ports); err != nil {
			if router.IsRetryable(err) {
				logger.Info("router temporarily unavailable during update, requeueing", "service", serviceKey, "error", err)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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

func (r *ServiceReconciler) patchStatus(ctx context.Context, svc *corev1.Service, ip string) (ctrl.Result, error) {
	if ip == "" {
		return ctrl.Result{}, nil
	}

	currentIP := ""
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		currentIP = svc.Status.LoadBalancer.Ingress[0].IP
	}

	if currentIP == ip {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(svc.DeepCopy())
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
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
