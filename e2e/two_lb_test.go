//go:build e2e && linux

package e2e_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

// two_lb_test.go covers the 2-LB dimension: independent LBs each routing to
// their own backends, and the sub-case where both LBs publish the same
// external port but the per-LB nftables DNAT chains stay isolated (no
// cross-talk).
var _ = Describe("two-lb matrix", func() {
	It("allocates two distinct LB IPs and routes each to its own backends", func() {
		ctx, cancel := contextWithTimeout(180 * time.Second)
		defer cancel()

		lb1, lb2 := "two-lb-1", "two-lb-2"

		By("deploying backends: lb1 with 1 replica, lb2 with 3 replicas")
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lb1, 1)).To(Succeed())
		defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lb1) }()
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lb2, 3)).To(Succeed())
		defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lb2) }()

		By("creating two LoadBalancer Services")
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lb1, 80, 8080)).To(Succeed())
		defer func() { _ = harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lb1) }()
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lb2, 80, 8080)).To(Succeed())
		defer func() { _ = harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lb2) }()

		By("waiting for both Services to receive distinct status IPs")
		ip1, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lb1, 60*time.Second)
		Expect(err).NotTo(HaveOccurred())
		ip2, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lb2, 60*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(ip1).NotTo(Equal(ip2), "the two LBs must have distinct IPs")

		By("verifying lb1 routes to exactly one backend")
		ids1, err := harness.CollectBackendIDs(ctx, ip1+":80", 20, 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(harness.AssertAllHit(ids1, 1)).To(Succeed(),
			"lb1 should hit only its single backend")

		By("verifying lb2 round-robins across 3 backends")
		ids2, err := harness.CollectBackendIDs(ctx, ip2+":80", 20, 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(harness.AssertAllHit(ids2, 3)).To(Succeed(),
			"lb2 should hit all 3 backends")
		Expect(harness.AssertRoughlyUniform(ids2, 0.25)).To(Succeed(),
			"lb2 traffic distribution should be roughly uniform")

		By("asserting per-LB isolation: lb1's response set is disjoint from lb2's")
		set1, set2 := harness.DistinctIDs(ids1), harness.DistinctIDs(ids2)
		for _, a := range set1 {
			for _, b := range set2 {
				Expect(a).NotTo(Equal(b), "lb1 and lb2 must not share backends")
			}
		}
	})

	It("distinguishes DNAT rules across LBs even with a shared external port", func() {
		ctx, cancel := contextWithTimeout(120 * time.Second)
		defer cancel()

		lb1, lb2 := "two-lb-shared-1", "two-lb-shared-2"

		By("deploying two single-replica backends with distinct label selectors")
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lb1, 1)).To(Succeed())
		defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lb1) }()
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lb2, 1)).To(Succeed())
		defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lb2) }()

		By("creating two LB Services on the same external port 80")
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lb1, 80, 8080)).To(Succeed())
		defer func() { _ = harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lb1) }()
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lb2, 80, 8080)).To(Succeed())
		defer func() { _ = harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lb2) }()

		ip1, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lb1, 60*time.Second)
		Expect(err).NotTo(HaveOccurred())
		ip2, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lb2, 60*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(ip1).NotTo(Equal(ip2), "LBs must have distinct IPs even on shared external port")

		By("hitting lb1 20 times — should see only lb1's backend")
		ids1, err := harness.CollectBackendIDs(ctx, ip1+":80", 20, 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(harness.AssertAllHit(ids1, 1)).To(Succeed(), "lb1 should hit only its own backend")

		By("hitting lb2 20 times — should see only lb2's backend")
		ids2, err := harness.CollectBackendIDs(ctx, ip2+":80", 20, 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(harness.AssertAllHit(ids2, 1)).To(Succeed(), "lb2 should hit only its own backend")

		// The two backends must be distinct — otherwise the test is meaningless.
		Expect(harness.DistinctIDs(ids1)[0]).NotTo(Equal(harness.DistinctIDs(ids2)[0]),
			"cross-talk detected: lb1 and lb2 hit the same backend despite shared external port")

		By(fmt.Sprintf("both LBs have distinct per-service nftables chains: lb1=%s, lb2=%s",
			harness.DistinctIDs(ids1), harness.DistinctIDs(ids2)))
	})
})
