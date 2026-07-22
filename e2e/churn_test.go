//go:build e2e && linux

package e2e_test

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

// churn_test.go exercises the Delete → recreate lifecycle. After delete:
//   - The macvlan interface disappears from `ip link` (the ingress's
//     NetlinkManager.DeleteAll path is invoked for the service).
//   - The per-service nftables chain is gone.
//   - The finalizer is removed from the Service.
// After recreate within 10s: state reconverges and traffic flows within 30s.
var _ = ginkgo.Describe("churn — delete then recreate", func() {
	ginkgo.It("leaves no leaked macvlan or nftables rules after delete", func() {
		ctx, cancel := contextWithTimeout(180 * time.Second)
		defer cancel()

		lbName := "churn-lb"

		ginkgo.By("deploying 1 backend + LB Service and waiting for status IP")
		gomega.Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, 1)).To(gomega.Succeed())
		defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lbName) }()
		gomega.Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(gomega.Succeed())

		ip, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ip).NotTo(gomega.BeEmpty())

		ginkgo.By("verifying traffic flows before delete")
		ids, err := harness.CollectBackendIDs(ctx, ip+":80", 10, 15*time.Second)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(harness.AssertAllHit(ids, 1)).To(gomega.Succeed())

		ginkgo.By("deleting the LB Service")
		gomega.Expect(harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName)).To(gomega.Succeed())

		ginkgo.By("waiting for the finalizer to be removed (≤ 60s)")
		gomega.Expect(harness.WaitForServiceNoFinalizer(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)).To(gomega.Succeed())

		ginkgo.By("waiting for ingress to tear down the macvlan and per-service nftables chain")
		gomega.Eventually(func(g gomega.Gomega) {
			health, err := sharedRouter().Health(ctx, healthReq())
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(health.GetActiveMacvlans()).To(gomega.BeZero(), "expected zero active macvlans after delete")
		}, 30*time.Second, 500*time.Millisecond).Should(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(harness.AssertNoLBChains()).To(gomega.Succeed(),
				"per-service nftables chains must be gone after delete")
		}, 30*time.Second, 500*time.Millisecond).Should(gomega.Succeed())
	})

	ginkgo.It("reconverges within 30s after recreate", func() {
		ctx, cancel := contextWithTimeout(180 * time.Second)
		defer cancel()

		lbName := "churn-recreate-lb"

		ginkgo.By("deploying 1 backend + LB Service")
		gomega.Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, 1)).To(gomega.Succeed())
		defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lbName) }()
		gomega.Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(gomega.Succeed())

		ip1, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("deleting and immediately recreating the LB Service")
		gomega.Expect(harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName)).To(gomega.Succeed())
		gomega.Expect(harness.WaitForServiceNoFinalizer(ctx, sharedK8s(), e2eNamespace, lbName, 30*time.Second)).To(gomega.Succeed())
		gomega.Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(gomega.Succeed())

		ginkgo.By("waiting for reconverge: status IP allocated and traffic flowing within 30s")
		var ip2 string
		gomega.Eventually(func(g gomega.Gomega) {
			svc, err := harness.GetService(ctx, sharedK8s(), e2eNamespace, lbName)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			ip2 = harness.ServiceStatusIP(svc)
			g.Expect(ip2).NotTo(gomega.BeEmpty(), "expected a freshly allocated status IP after recreate")
		}, 30*time.Second, 1*time.Second).Should(gomega.Succeed())

		// The recreated LB IP is very likely the same (dnsmasq hands back the
		// same address inside its lease window) but the test only requires
		// *some* reachable IP. We assert traffic flows regardless.
		gomega.Eventually(func(g gomega.Gomega) {
			ids, err := harness.CollectBackendIDs(ctx, ip2+":80", 10, 15*time.Second)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(harness.AssertAllHit(ids, 1)).To(gomega.Succeed(),
				"traffic must flow to the backend after recreate")
			_ = ip1 // silence -unused if assertion short-circuits
		}, 30*time.Second, 2*time.Second).Should(gomega.Succeed())

		ginkgo.By("cleanup")
		_ = harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName)
	})
})
