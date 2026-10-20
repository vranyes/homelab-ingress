package harness

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	e2eNamespace = "hl-ingress-e2e"

	// backendImage must match the reference pre-pulled by
	// e2e-vm/scripts/04-backend-image.sh. Keep the tag explicit — an
	// untagged reference silently resolves to :latest at pull time but
	// reads ambiguously in failure messages (e.g. ImagePullBackOff).
	backendImage = "traefik/whoami:latest"
	backendPort  = 8080

	lbClassHomelabIngress = "homelab-ingress"
	// finalizerKey must match github.com/vranyes/homelab-ingress/controller/internal/controller.Finalizer.
	// The controller's AnnotationPrefix is "homelab-ingress.vranyes.com".
	finalizerKey = "homelab-ingress.vranyes.com/cleanup"
)

var k8sScheme *runtime.Scheme

func init() {
	k8sScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(k8sScheme)
	_ = appsv1.AddToScheme(k8sScheme)
	_ = corev1.AddToScheme(k8sScheme)
	_ = discoveryv1.AddToScheme(k8sScheme)
}

// NewClient returns a controller-runtime client bound to the in-VM k3s cluster.
// KUBECONFIG is read from env via config.GetConfig().
func NewClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("get kubeconfig: %w", err)
	}
	return client.New(cfg, client.Options{Scheme: k8sScheme})
}

// EnsureNamespace idempotently creates the e2e namespace.
func EnsureNamespace(ctx context.Context, cl client.Client, name string) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	k := types.NamespacedName{Name: name}
	err := cl.Get(ctx, k, ns)
	if apierrors.IsNotFound(err) {
		if err := cl.Create(ctx, ns); err != nil {
			return fmt.Errorf("create namespace: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get namespace: %w", err)
	}
	if ns.ObjectMeta.DeletionTimestamp != nil {
		return fmt.Errorf("namespace %s is being deleted", name)
	}
	return nil
}

// DeleteNamespaceIfExists removes a namespace if it exists. Best effort.
func DeleteNamespaceIfExists(ctx context.Context, cl client.Client, name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := cl.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// DeployIdenticon creates a Deployment of backend pods (traefik/whoami).
// name is used as the deployment name and pod labels. The caller is
// responsible for creating the LoadBalancer Service separately (see
// CreateLBService). Kubernetes automatically generates EndpointSlices for
// the LB Service because it has a selector.
func DeployIdenticon(ctx context.Context, cl client.Client, ns, name string, replicas int32) error {
	labels := map[string]string{"app": name}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: int64Ptr(1),
					Containers: []corev1.Container{{
						Name:  "whoami",
						Image: backendImage,
						Args:  []string{"--port=8080"},
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: backendPort, Protocol: corev1.ProtocolTCP}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("16Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
						},
					}},
				},
			},
		},
	}
	if err := cl.Create(ctx, deployment); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create deployment: %w", err)
	}
	return nil
}

// CreateLBService creates a LoadBalancer Service with loadBalancerClass
// "homelab-ingress". externalPort is the port advertised externally;
// targetPort is the pod port. Kubernetes auto-generates EndpointSlices for
// this Service because it has a selector. The controller's EndpointSlice
// watcher picks them up by the standard kubernetes.io/service-name label.
func CreateLBService(ctx context.Context, cl client.Client, ns, name string, externalPort, targetPort int32) error {
	labels := map[string]string{"app": name}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass: stringPtr(lbClassHomelabIngress),
			Selector:          labels,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       externalPort,
				TargetPort: intstr.FromInt(int(targetPort)),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	if err := cl.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create lb service: %w", err)
	}
	return nil
}

// DeleteService deletes a Service by name. Returns nil if not found.
func DeleteService(ctx context.Context, cl client.Client, ns, name string) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := cl.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// DeleteDeployment deletes a Deployment by name. Best effort.
func DeleteDeployment(ctx context.Context, cl client.Client, ns, name string) error {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := cl.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// GetService retrieves a Service by name.
func GetService(ctx context.Context, cl client.Client, ns, name string) (*corev1.Service, error) {
	svc := &corev1.Service{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, svc); err != nil {
		return nil, err
	}
	return svc, nil
}

// ServiceStatusIP returns the first ingress IP from a Service's status, or
// empty string if not yet populated.
func ServiceStatusIP(svc *corev1.Service) string {
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		return ""
	}
	return svc.Status.LoadBalancer.Ingress[0].IP
}

// WaitForServiceStatusIP polls the Service until its status.loadBalancer.ingress[0].ip
// is non-empty, returning the IP. timeout is the maximum wait.
func WaitForServiceStatusIP(ctx context.Context, cl client.Client, ns, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		svc, err := GetService(ctx, cl, ns, name)
		if err != nil {
			return "", err
		}
		if ip := ServiceStatusIP(svc); ip != "" {
			return ip, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("timeout waiting for service %s/%s status IP", ns, name)
}

// WaitForServiceFinalizer polls the Service until the homelab-ingress finalizer
// is present. timeout is the maximum wait.
func WaitForServiceFinalizer(ctx context.Context, cl client.Client, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		svc, err := GetService(ctx, cl, ns, name)
		if err != nil {
			return err
		}
		if HasFinalizer(svc) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for service %s/%s finalizer", ns, name)
}

// WaitForServiceNoFinalizer polls until the finalizer is gone (post-delete reconcile).
func WaitForServiceNoFinalizer(ctx context.Context, cl client.Client, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		svc, err := GetService(ctx, cl, ns, name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if !HasFinalizer(svc) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for service %s/%s finalizer to be removed", ns, name)
}

// HasFinalizer reports whether the service carries the homelab-ingress cleanup finalizer.
func HasFinalizer(svc *corev1.Service) bool {
	for _, f := range svc.Finalizers {
		if f == finalizerKey {
			return true
		}
	}
	return false
}

// ScaleDeployment updates the replica count of a Deployment.
func ScaleDeployment(ctx context.Context, cl client.Client, ns, name string, replicas int32) error {
	dep := &appsv1.Deployment{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, dep); err != nil {
		return err
	}
	dep.Spec.Replicas = &replicas
	return cl.Update(ctx, dep)
}

func int64Ptr(i int64) *int64    { return &i }
func stringPtr(s string) *string { return &s }
