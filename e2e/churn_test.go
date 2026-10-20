//go:build e2e && linux

package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

// churn_test.go exercises the Delete → recreate lifecycle. After delete:
//   - The macvlan interface disappears from `ip link` (the ingress's
//     NetlinkManager.DeleteAll path is invoked for the service).
//   - The per-service nftables chain is gone.
//   - The finalizer is removed from the Service.
//
// After recreate within 10s: state reconverges and traffic flows within 30s.
var _ = Describe("churn — delete then recreate", func() {
	It("leaves no leaked macvlan or nftables rules after delete", func() {
		ctx, cancel := contextWithTimeout(180 * time.Second)
		defer cancel()

		lbName := "churn-lb"

		By("deploying 1 backend + LB Service and waiting for status IP")
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, 1)).To(Succeed())
		defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lbName) }()
		By("waiting for backend pods to be Ready (fails fast on ImagePullBackOff)")
		Expect(harness.WaitForBackendReady(ctx, sharedK8s(), e2eNamespace, lbName, 90*time.Second)).To(Succeed())
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())

		ip, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(ip).NotTo(BeEmpty())

		By("verifying traffic flows before delete (fresh connections)")
		ids, err := collectIDs(ctx, ip+":80", 10, 15*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(harness.AssertAllHit(ids, 1)).To(Succeed())

		By("deleting the LB Service")
		Expect(harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName)).To(Succeed())

		By("waiting for the finalizer to be removed (≤ 60s)")
		Expect(harness.WaitForServiceNoFinalizer(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)).To(Succeed())

		By("waiting for ingress to tear down the macvlan and per-service nftables chain")
		Eventually(func(g Gomega) {
			health, err := sharedRouter().Health(ctx, healthReq())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(health.GetActiveMacvlans()).To(BeZero(), "expected zero active macvlans after delete")
		}, 30*time.Second, 500*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(harness.AssertNoLBChains()).To(Succeed(),
				"per-service nftables chains must be gone after delete")
		}, 30*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("reconverges within 30s after recreate", func() {
		ctx, cancel := contextWithTimeout(180 * time.Second)
		defer cancel()

		lbName := "churn-recreate-lb"

		By("deploying 1 backend + LB Service")
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, 1)).To(Succeed())
		defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lbName) }()
		By("waiting for backend pods to be Ready (fails fast on ImagePullBackOff)")
		Expect(harness.WaitForBackendReady(ctx, sharedK8s(), e2eNamespace, lbName, 90*time.Second)).To(Succeed())
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())

		ip1, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)
		Expect(err).NotTo(HaveOccurred())

		By("deleting and immediately recreating the LB Service")
		Expect(harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName)).To(Succeed())
		Expect(harness.WaitForServiceNoFinalizer(ctx, sharedK8s(), e2eNamespace, lbName, 30*time.Second)).To(Succeed())
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())

		By("waiting for reconverge: status IP allocated and traffic flowing within 30s")
		var ip2 string
		Eventually(func(g Gomega) {
			svc, err := harness.GetService(ctx, sharedK8s(), e2eNamespace, lbName)
			g.Expect(err).NotTo(HaveOccurred())
			ip2 = harness.ServiceStatusIP(svc)
			g.Expect(ip2).NotTo(BeEmpty(), "expected a freshly allocated status IP after recreate")
		}, 30*time.Second, 1*time.Second).Should(Succeed())

		// The recreated LB IP is very likely the same (dnsmasq hands back the
		// same address inside its lease window) but the test only requires
		// *some* reachable IP. We assert traffic flows regardless.
		Eventually(func(g Gomega) {
			ids, err := collectIDs(ctx, ip2+":80", 10, 15*time.Second)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(harness.AssertAllHit(ids, 1)).To(Succeed(),
				"traffic must flow to the backend after recreate")
			_ = ip1 // silence -unused if assertion short-circuits
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("cleanup")
		_ = harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName)
	})
})
