package harness

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeployBackend wraps EnsureNamespace (idempotent) and DeployIdenticon into a
// single deploy call for scenarios. Callers must create the LoadBalancer
// Service separately via CreateLBService after DeployBackend returns.
// The Deployment selector label is `app=<name>`. The LB Service (created by
// the caller with CreateLBService) uses the same label selector so Kubernetes
// generates EndpointSlices automatically.
func DeployBackend(ctx context.Context, cl client.Client, ns, name string, replicas int32) error {
	if err := EnsureNamespace(ctx, cl, ns); err != nil {
		return fmt.Errorf("create namespace: %w", err)
	}
	if err := DeployIdenticon(ctx, cl, ns, name, replicas); err != nil {
		return fmt.Errorf("create deployment: %w", err)
	}
	return nil
}

// TearDownBackend removes the backend Deployment named `<name>` in `ns`. It
// does NOT delete the namespace so that it can be shared across scenarios —
// the suite's AfterAll handles namespace cleanup.
func TearDownBackend(ctx context.Context, cl client.Client, ns, name string) error {
	if err := DeleteService(ctx, cl, ns, name); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	if err := DeleteDeployment(ctx, cl, ns, name); err != nil {
		return fmt.Errorf("delete deployment: %w", err)
	}
	return nil
}
