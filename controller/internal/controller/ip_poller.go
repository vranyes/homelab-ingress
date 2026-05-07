package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vranyes/homelab-ingress/controller/internal/router"
)

type IPPoller struct {
	client.Client
	RouterClient router.Client
	Interval     time.Duration
}

func (p *IPPoller) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("ip-poller")
	log.Info("starting IP change poller", "interval", p.Interval)

	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("stopping IP change poller")
			return nil
		case <-ticker.C:
			if err := p.poll(ctx); err != nil {
				log.Error(err, "IP poll failed")
			}
		}
	}
}

func (p *IPPoller) NeedLeaderElection() bool {
	return true
}

func (p *IPPoller) poll(ctx context.Context) error {
	log := ctrl.Log.WithName("ip-poller")

	entries, err := p.RouterClient.List(ctx)
	if err != nil {
		return fmt.Errorf("list router entries: %w", err)
	}

	updated := 0
	for _, entry := range entries {
		if entry.AssignedIP == "" {
			continue
		}

		parts := splitServiceKey(entry.ServiceKey)
		if parts.namespace == "" || parts.name == "" {
			continue
		}

		var svc corev1.Service
		if err := p.Get(ctx, types.NamespacedName{Namespace: parts.namespace, Name: parts.name}, &svc); err != nil {
			log.V(1).Info("service not found for router entry", "service", entry.ServiceKey)
			continue
		}

		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}

		currentIP := ""
		if len(svc.Status.LoadBalancer.Ingress) > 0 {
			currentIP = svc.Status.LoadBalancer.Ingress[0].IP
		}

		if currentIP != entry.AssignedIP {
			log.Info("detected IP change", "service", entry.ServiceKey, "old", currentIP, "new", entry.AssignedIP)
			patch := client.MergeFrom(svc.DeepCopy())
			svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: entry.AssignedIP}}
			if err := p.Status().Patch(ctx, &svc, patch); err != nil {
				log.Error(err, "failed to patch service status", "service", entry.ServiceKey)
				continue
			}
			updated++
		}
	}

	if updated > 0 {
		log.Info("IP poll complete", "updated", updated)
	}
	return nil
}

type serviceKeyParts struct {
	namespace string
	name      string
}

func splitServiceKey(key string) serviceKeyParts {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return serviceKeyParts{namespace: key[:i], name: key[i+1:]}
		}
	}
	return serviceKeyParts{}
}
