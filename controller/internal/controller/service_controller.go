package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
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
	matches := func(obj any) bool {
		svc, ok := obj.(*corev1.Service)
		if !ok || svc == nil {
			return false
		}
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			return false
		}
		// Read spec.loadBalancerClass via unstructured conversion so we don't
		// require the field to exist at compile time in the client lib.
		m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(svc)
		if err != nil {
			return false
		}
		if v, found, err := unstructured.NestedString(m, "spec", "loadBalancerClass"); err == nil && found {
			return v == "homelab-lb"
		}
		return false
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}). // Primary resource
		WithEventFilter(predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return matches(e.Object) },
			UpdateFunc:  func(e event.UpdateEvent) bool { return matches(e.ObjectOld) || matches(e.ObjectNew) },
			DeleteFunc:  func(e event.DeleteEvent) bool { return matches(e.Object) },
			GenericFunc: func(e event.GenericEvent) bool { return matches(e.Object) },
		}).
		Watches(
			&discoveryv1.EndpointSlice{}, // Watch this secondary resource
			handler.EnqueueRequestsFromMapFunc(r.findServiceForEndpointSlice),
		).
		Complete(r)
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

func (r *ServiceReconciler) reconcileUpsert(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	serviceKey := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}.String()

	ips, namedPorts, err := r.endpointInfoForService(ctx, svc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list endpoints: %w", err)
	}
	if len(ips) == 0 {
		logger.Info("no ready endpoint IPs yet, requeueing")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	ports := buildPortMappings(svc, ips, namedPorts)
	// If any mapping couldn't resolve a named target port (PodPort == 0), requeue
	for _, m := range ports {
		if m.PodPort == 0 {
			logger.Info("no target port resolved for named port yet, requeueing")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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
		return r.patchStatus(ctx, svc, entry.AssignedIP)
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

func (r *ServiceReconciler) endpointIPsForService(ctx context.Context, svc *corev1.Service) ([]string, error) {
	// Select EndpointSlices for this Service by the standard label.
	sel := labels.SelectorFromSet(labels.Set{"kubernetes.io/service-name": svc.Name})
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices, &client.ListOptions{
		Namespace:     svc.Namespace,
		LabelSelector: sel,
	}); err != nil {
		return nil, err
	}

	var ips []string
	for _, slice := range slices.Items {
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
	}
	return ips, nil
}

// endpointInfoForService returns the ready endpoint IPs for a Service and a
// mapping of named ports to numeric ports discovered from EndpointSlices in the
// same namespace for that Service. The namedPorts map is keyed by port name
// and contains the numeric port that endpoints advertise for that name. This
// allows resolving Service ports that target a named port rather than a
// numeric TargetPort.
func (r *ServiceReconciler) endpointInfoForService(ctx context.Context, svc *corev1.Service) ([]string, map[string]int32, error) {
	// Select EndpointSlices for this Service by the standard label.
	sel := labels.SelectorFromSet(labels.Set{"kubernetes.io/service-name": svc.Name})
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices, &client.ListOptions{
		Namespace:     svc.Namespace,
		LabelSelector: sel,
	}); err != nil {
		return nil, nil, err
	}

	var ips []string
	namedPorts := make(map[string]int32)
	for _, slice := range slices.Items {
		// Record ports advertised by this slice. EndpointSlice.Ports may contain
		// a name and port number. We'll prefer the first numeric value seen for
		// each name.
		for _, sp := range slice.Ports {
			if sp.Name != nil && sp.Port != nil {
				name := *sp.Name
				if _, exists := namedPorts[name]; !exists {
					namedPorts[name] = int32(*sp.Port)
				}
			}
		}

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
	}
	// If we didn't find any EndpointSlices (common in older clusters or tests),
	// fall back to listing Pods by the Service selector to collect IPs.
	if len(ips) == 0 && len(svc.Spec.Selector) > 0 {
		var pods corev1.PodList
		sel := labels.SelectorFromSet(labels.Set(svc.Spec.Selector))
		if err := r.List(ctx, &pods, &client.ListOptions{Namespace: svc.Namespace, LabelSelector: sel}); err != nil {
			return nil, nil, err
		}
		for _, p := range pods.Items {
			// consider only running/ready pods with an IP
			if p.Status.PodIP == "" {
				continue
			}
			// prefer the Pod Ready condition if present
			ready := false
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					ready = true
					break
				}
			}
			if !ready {
				continue
			}
			ips = append(ips, p.Status.PodIP)
		}
	}

	return ips, namedPorts, nil
}

// buildPortMappings constructs router.PortMapping entries for the Service
// ports. If a ServicePort.TargetPort is a name, the function consults the
// namedPorts map (from EndpointSlices) to resolve the numeric pod port. If a
// named port is not found, PodPort will be 0 to indicate unresolved.
func buildPortMappings(svc *corev1.Service, podIPs []string, namedPorts map[string]int32) []router.PortMapping {
	var mappings []router.PortMapping
	for _, p := range svc.Spec.Ports {
		proto := "tcp"
		if p.Protocol == corev1.ProtocolUDP {
			proto = "udp"
		}

		var podPort int32
		if p.TargetPort.Type == intstr.Int {
			podPort = int32(p.TargetPort.IntValue())
		} else if p.TargetPort.Type == intstr.String {
			name := p.TargetPort.StrVal
			if name != "" {
				if v, ok := namedPorts[name]; ok {
					podPort = v
				} else {
					// unresolved named port, leave podPort as 0
					podPort = 0
				}
			}
		}

		mappings = append(mappings, router.PortMapping{
			Protocol:     proto,
			ExternalPort: int32(p.Port),
			PodPort:      podPort,
			PodIPs:       podIPs,
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
		if existing[i].PodPort != desired[i].PodPort {
			return true
		}
		if !stringSliceEqual(existing[i].PodIPs, desired[i].PodIPs) {
			return true
		}
	}
	return false
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int)
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		if m[v] == 0 {
			return false
		}
		m[v]--
	}
	return true
}
