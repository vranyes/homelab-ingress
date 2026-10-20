//go:build e2e && linux

package e2e_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

// one_lb_test.go covers the 1-LB dimension of the scenario matrix with three
// target counts: 0, 1, and 3 (representing 2+).
//
// For each entry:
//   - 0 targets: Deploy no backend and create the LB Service. The controller
//     requeues every 5s without writing status.LoadBalancer.Ingress because
//     endpointInfoForService finds no ready endpoints, so no
//     CreateLoadBalancer RPC ever reaches the ingress daemon: no macvlan is
//     provisioned, no DHCP lease is acquired, no DNAT rule exists, and only
//     the finalizer is added to the Service (see
//     controller/internal/controller::(*ServiceReconciler).reconcileUpsert).
//     Assert all three zero-state invariants.
//   - 1 target: A single backend pod; 50 fresh-conn GETs all return the same backend ID.
//   - 2+ targets: Three backend pods; 50 fresh-conn GETs return all 3 backend IDs
//     distributed roughly uniformly (within ±25%).
//
// Traffic always uses fresh TCP connections (DisableKeepAlives): keep-alive
// reuses the same connection — and thus the same numgen evaluation — across
// requests, which under-counts distinct backends on round-robin rules.
var _ = Describe("one-lb matrix", func() {
	DescribeTable("1 LB, N targets",
		func(replicas int32, expectedDistinct int) {
			lbName := fmt.Sprintf("one-lb-%d", replicas)
			ctx, cancel := contextWithTimeout(240 * time.Second)
			DeferCleanup(cancel)

			By(fmt.Sprintf("deploying %d identicon backend(s) for %s", replicas, lbName))
			if replicas > 0 {
				Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, replicas)).To(Succeed())
				DeferCleanup(harness.TearDownBackend, ctx, sharedK8s(), e2eNamespace, lbName)
				By("waiting for backend pods to be Ready (fails fast on ImagePullBackOff)")
				Expect(harness.WaitForBackendReady(ctx, sharedK8s(), e2eNamespace, lbName, 90*time.Second)).To(Succeed())
			} else {
				Eventually(harness.EnsureNamespace).
					WithTimeout(5*time.Second).
					WithArguments(ctx, sharedK8s(), e2eNamespace).
					Should(Succeed())
			}

			By(fmt.Sprintf("creating LoadBalancer Service %s", lbName))
			Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())
			DeferCleanup(harness.DeleteService, ctx, sharedK8s(), e2eNamespace, lbName)

			By("waiting for the homelab-ingress finalizer to land")
			Expect(harness.WaitForServiceFinalizer(ctx, sharedK8s(), e2eNamespace, lbName, 30*time.Second)).To(Succeed())

			if replicas == 0 {
				// With no ready endpoints the controller requeues indefinitely
				// without ever calling CreateLoadBalancer on the router, so
				// the ingress daemon never provisions a macvlan interface,
				// no DHCP lease is acquired, no status IP is written, and
				// only the finalizer is added to the Service (see
				// controller/internal/controller::(*ServiceReconciler).reconcileUpsert).
				// Assert all three zero-state invariants.
				By("asserting no service status IP, no nftables chains, and zero macvlans when there are no backends")
				consistentlyNoStatusIP(ctx, e2eNamespace, lbName, 10*time.Second)
				Consistently(func(g Gomega) {
					health, err := sharedRouter().Health(ctx, healthReq())
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(health.GetActiveMacvlans()).To(BeZero(),
						"with 0 backends the controller must not provision a macvlan")
				}, 15*time.Second, 500*time.Millisecond).Should(Succeed())
				Eventually(func(g Gomega) {
					Expect(harness.AssertNoLBChains()).To(Succeed(),
						"with 0 backends nftables should have no per-service chains")
				}, 15*time.Second, 500*time.Millisecond).Should(Succeed())
				return
			}

			By("waiting for the LoadBalancer Service to receive a status IP")
			lbIP, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)
			Expect(err).NotTo(HaveOccurred())
			Expect(lbIP).NotTo(BeEmpty())

			By(fmt.Sprintf("firing 50 HTTP GETs (fresh connections) against %s:80 expecting %d distinct backends", lbIP, expectedDistinct))
			Eventually(func(g Gomega) {
				ids, err := harness.CollectBackendIDsFreshConn(ctx, lbIP+":80", 50, 60*time.Second)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ids).To(HaveLen(50), "expected all 50 requests to hit a backend")
				g.Expect(harness.AssertAllHit(ids, expectedDistinct)).To(Succeed(),
					"distinct backend count != expected")
				if expectedDistinct >= 2 {
					g.Expect(harness.AssertRoughlyUniform(ids, 0.25)).To(Succeed(),
						"distribution not within ±25%% uniform")
				}
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		},
		Entry("0 targets — no backends", int32(0), 0),
		Entry("1 target", int32(1), 1),
		Entry("2+ targets (3 replicas)", int32(3), 3),
	)
})

// consistentlyNoStatusIP asserts the LoadBalancer Service never gets a status
// IP within the supplied window. Cancellation via ctx aborts the poll so a
// suite timeout does not sit out the full window.
func consistentlyNoStatusIP(ctx context.Context, ns, name string, window time.Duration) {
	deadline := time.Now().Add(window)
	for deadline.After(time.Now()) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		svc, err := harness.GetService(ctx, sharedK8s(), ns, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(harness.ServiceStatusIP(svc)).To(BeEmpty(),
			"Service %s/%s should not have a status IP with 0 endpoints", ns, name)
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
