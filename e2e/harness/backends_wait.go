package harness

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WaitForBackendReady polls until the backend Deployment `<name>` in `ns`
// has its desired replica count Available and at least that many Pods with
// the Ready condition true.
//
// Rationale: DeployBackend only creates the Deployment. Without this wait,
// a missing backend image (e.g. k3s containerd cannot reach Docker Hub —
// see e2e-vm/scripts/04-backend-image.sh) surfaces 60s later as a
// WaitForServiceStatusIP timeout, which misattributes an infra problem to
// the ingress data plane. This helper fails fast with the pod's container
// waiting reason (e.g. ImagePullBackOff) so the cause is obvious.
func WaitForBackendReady(ctx context.Context, cl client.Client, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	selector := labels.SelectorFromSet(labels.Set{"app": name})
	var lastErr error
	for time.Now().Before(deadline) {
		// 1) Deployment availability.
		dep := &appsv1.Deployment{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, dep); err != nil {
			lastErr = fmt.Errorf("get deployment %s/%s: %w", ns, name, err)
		} else {
			desired := int32(1)
			if dep.Spec.Replicas != nil {
				desired = *dep.Spec.Replicas
			}
			if dep.Status.AvailableReplicas >= desired && desired > 0 {
				// 2) Pod-level readiness (guards against Available lagging).
				var pods corev1.PodList
				if err := cl.List(ctx, &pods, &client.ListOptions{
					Namespace:     ns,
					LabelSelector: selector,
				}); err != nil {
					lastErr = fmt.Errorf("list backend pods: %w", err)
				} else {
					ready := 0
					for i := range pods.Items {
						if podReady(&pods.Items[i]) {
							ready++
						}
					}
					if int32(ready) >= desired {
						return nil
					}
					lastErr = fmt.Errorf("deployment %s/%s: %d/%d pods ready (%s)",
						ns, name, ready, desired, summarizePods(pods.Items))
				}
			} else {
				lastErr = fmt.Errorf("deployment %s/%s: %d/%d available",
					ns, name, dep.Status.AvailableReplicas, desired)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("unknown")
	}
	return fmt.Errorf("timeout waiting for backend %s/%s ready after %s: last state: %v",
		ns, name, timeout, lastErr)
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// summarizePods renders a compact one-line-per-pod summary including any
// container waiting reason (e.g. ImagePullBackOff) so provisioning failures
// are actionable without `kubectl describe`.
func summarizePods(pods []corev1.Pod) string {
	parts := make([]string, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		reasons := make([]string, 0, len(p.Status.ContainerStatuses))
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				r := cs.State.Waiting.Reason
				msg := cs.State.Waiting.Message
				if len(msg) > 120 {
					msg = msg[:120] + "..."
				}
				if msg != "" {
					r += ": " + msg
				}
				reasons = append(reasons, r)
			} else if !cs.Ready {
				reasons = append(reasons, "not-ready")
			}
		}
		parts = append(parts, fmt.Sprintf("%s phase=%s ready=%v [%s]",
			p.Name, p.Status.Phase, podReady(p), strings.Join(reasons, ",")))
	}
	return strings.Join(parts, "; ")
}
