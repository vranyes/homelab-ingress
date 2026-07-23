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
//     requeues every 5s without writing status.LoadBalancer.Ingress. The
//     LB itself (macvlan + dnsmasq lease + nftables chain) is created on the
//     ingress side, but no DNAT rule exists, so HTTP GETs to the LB IP fail.
//   - 1 target: A single backend pod; 20 GETs all return the same backend ID.
//   - 2+ targets: Three backend pods; 20 GETs return ≥2 distinct backend IDs
//     distributed roughly uniformly (within ±25%).
var _ = Describe("one-lb matrix", func() {
	DescribeTable("1 LB, N targets",
		func(replicas int32, expectedDistinct int) {
			lbName := fmt.Sprintf("one-lb-%d", replicas)
			ctx, cancel := contextWithTimeout(120 * time.Second)
			defer cancel()

			By(fmt.Sprintf("deploying %d identicon backend(s) for %s", replicas, lbName))
			if replicas > 0 {
				Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, replicas)).To(Succeed())
				defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lbName) }()
			} else {
				Expect(harness.EnsureNamespace(ctx, sharedK8s(), e2eNamespace)).To(Succeed())
			}

			By(fmt.Sprintf("creating LoadBalancer Service %s", lbName))
			Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())
			defer func() { _ = harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName) }()

			By("waiting for the homelab-ingress finalizer to land")
			Expect(harness.WaitForServiceFinalizer(ctx, sharedK8s(), e2eNamespace, lbName, 30*time.Second)).To(Succeed())

			if replicas == 0 {
				// With no backends the controller requeues indefinitely without
				// writing service status. Assert the macvlan has been created
				// on the ingress side (via Health), but a status IP is NEVER set.
				By("asserting no service status IP and an active macvlan on the ingress side")
				consistentlyNoStatusIP(ctx, e2eNamespace, lbName, 10*time.Second)
				Eventually(func(g Gomega) {
					health, err := sharedRouter().Health(ctx, healthReq())
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(health.GetActiveMacvlans()).To(BeNumerically(">=", int32(1)),
						"expected an active macvlan even with 0 targets")
				}, 30*time.Second, 500*time.Millisecond).Should(Succeed())
				return
			}

			By("waiting for the LoadBalancer Service to receive a status IP")
			lbIP, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)
			Expect(err).NotTo(HaveOccurred())
			Expect(lbIP).NotTo(BeEmpty())

			By(fmt.Sprintf("firing 20 HTTP GETs against %s:80 expecting %d distinct backends", lbIP, expectedDistinct))
			Eventually(func(g Gomega) {
				ids, err := harness.CollectBackendIDs(ctx, lbIP+":80", 20, 30*time.Second)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ids).To(HaveLen(20), "expected all 20 requests to hit a backend")
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
// IP within the supplied window.
func consistentlyNoStatusIP(ctx context.Context, ns, name string, window time.Duration) {
	_ = ctx // currently unused; kept for API symmetry with other helpers
	// We poll manually rather than using Eventually/Consistently so the failure
	// message points at the exact moment the IP appeared if it does.
	deadline := time.Now().Add(window)
	for deadline.After(time.Now()) {
		svc, err := harness.GetService(context.Background(), sharedK8s(), ns, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(harness.ServiceStatusIP(svc)).To(BeEmpty(),
			"Service %s/%s should not have a status IP with 0 endpoints", ns, name)
		time.Sleep(500 * time.Millisecond)
	}
}
