//go:build e2e && linux

package e2e_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

const (
	rlHTTPPort    = 80
	rlBackendPort = 8080
)

func u64(v uint64) *uint64 { return &v }

// setupUnlimitedLB deploys one backend and a LoadBalancer Service without any
// rate-limit annotation.
func setupUnlimitedLB(ctx context.Context, name string) (string, string) {
	By("deploying backend for " + name)
	Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, name, 1)).To(Succeed())
	DeferCleanup(harness.TearDownBackend, ctx, sharedK8s(), e2eNamespace, name)
	Expect(harness.WaitForBackendReady(ctx, sharedK8s(), e2eNamespace, name, 90*time.Second)).To(Succeed())

	By("creating LoadBalancer Service " + name)
	Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, name, rlHTTPPort, rlBackendPort)).To(Succeed())
	DeferCleanup(harness.DeleteService, ctx, sharedK8s(), e2eNamespace, name)

	Expect(harness.WaitForServiceFinalizer(ctx, sharedK8s(), e2eNamespace, name, 30*time.Second)).To(Succeed())
	ip, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, name, 60*time.Second)
	Expect(err).NotTo(HaveOccurred())
	Expect(ip).NotTo(BeEmpty())
	return ip, serviceKey(name)
}

// waitForRateLimit asserts the rule, set and encoded parameters converged.
func waitForRateLimit(key string, rate uint64, burst uint32) {
	Eventually(func(g Gomega) {
		rules, err := harness.ClassifyRules(key)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(harness.CountRuleKind(rules, harness.RuleKindRateLimit)).To(Equal(1),
			"expected exactly one rate-limit rule")
		g.Expect(harness.AssertRateLimitSetExists(key)).To(Succeed())
		params, err := harness.RateLimitParamsFor(key)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(params.Rate).To(Equal(rate))
		g.Expect(params.Burst).To(Equal(burst))
	}).WithTimeout(60 * time.Second).WithPolling(time.Second).Should(Succeed())
}

// expectLimited asserts that a burst of new connections is throttled to the
// burst size (plus a small refill margin) and that the drop counter advanced.
func expectLimited(ctx context.Context, key, addr string, burst uint32) {
	before, ok := harness.CounterFor(mustClassify(key), harness.RuleKindRateLimit)
	Expect(ok).To(BeTrue(), "rate-limit rule missing before the flood")

	succeeded := 0
	Eventually(func(g Gomega) {
		var err error
		succeeded, err = harness.AttemptParallelFromNS(ctx, vepaClientNS, usSourceIP, addr, 30, 900*time.Millisecond)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(succeeded).To(BeNumerically(">=", 1), "no connection got through the bucket")
		g.Expect(succeeded).To(BeNumerically("<=", int(burst)+2),
			"expected the per-client limit to throttle 30 connections to ~%d, got %d", burst, succeeded)
	}).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

	Eventually(func(g Gomega) {
		after, ok := harness.CounterFor(mustClassify(key), harness.RuleKindRateLimit)
		g.Expect(ok).To(BeTrue())
		g.Expect(after.Packets).To(BeNumerically(">", before.Packets),
			"rate-limit drop counter did not advance")
	}).WithTimeout(15 * time.Second).WithPolling(time.Second).Should(Succeed())
}

var _ = Describe("rate limiting", func() {
	It("enforces, updates and removes a per-client connection limit", func() {
		ctx, cancel := contextWithTimeout(480 * time.Second)
		DeferCleanup(cancel)

		ip, key := setupUnlimitedLB(ctx, "rl-lifecycle")
		addr := ip + ":80"

		By("confirming the service works before any limit is configured")
		Eventually(func(g Gomega) {
			ids, err := harness.CollectBackendIDsFromNS(ctx, vepaClientNS, addr, 5, 20*time.Second)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ids).To(HaveLen(5))
		}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			rules, err := harness.ClassifyRules(key)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(harness.CountRuleKind(rules, harness.RuleKindRateLimit)).To(BeZero())
		}).WithTimeout(15 * time.Second).WithPolling(time.Second).Should(Succeed())

		By("adding rate=1/s burst=3 via the Service annotations")
		Expect(harness.SetRateLimit(ctx, sharedK8s(), e2eNamespace, "rl-lifecycle", u64(1), u64(3))).To(Succeed())
		waitForRateLimit(key, 1, 3)

		By("flooding 30 new connections from one client")
		expectLimited(ctx, key, addr, 3)

		By("confirming the bucket refills")
		Eventually(func(g Gomega) {
			n, err := harness.AttemptParallelFromNS(ctx, vepaClientNS, usSourceIP, addr, 1, 2*time.Second)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(n).To(Equal(1), "a single connection should pass once the bucket refills")
		}).WithTimeout(15 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

		By("changing the limit to rate=7/s burst=9")
		oldSets, err := harness.RateLimitSets(key)
		Expect(err).NotTo(HaveOccurred())
		Expect(oldSets).To(HaveLen(1))
		Expect(harness.SetRateLimit(ctx, sharedK8s(), e2eNamespace, "rl-lifecycle", u64(7), u64(9))).To(Succeed())
		waitForRateLimit(key, 7, 9)

		By("asserting the old bucket set was replaced")
		Eventually(func(g Gomega) {
			sets, err := harness.RateLimitSets(key)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sets).To(HaveLen(1))
			g.Expect(sets[0]).NotTo(Equal(oldSets[0]),
				"a rate change must start from a fresh bucket set")
		}).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		By("removing the annotations")
		Expect(harness.SetRateLimit(ctx, sharedK8s(), e2eNamespace, "rl-lifecycle", nil, nil)).To(Succeed())
		Eventually(func(g Gomega) {
			rules, err := harness.ClassifyRules(key)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(harness.CountRuleKind(rules, harness.RuleKindRateLimit)).To(BeZero())
			g.Expect(harness.AssertNoRateLimitSet(key)).To(Succeed())
		}).WithTimeout(60 * time.Second).WithPolling(time.Second).Should(Succeed())

		By("confirming traffic is unlimited again")
		Eventually(func(g Gomega) {
			n, err := harness.AttemptParallelFromNS(ctx, vepaClientNS, usSourceIP, addr, 30, 2*time.Second)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(n).To(Equal(30), "all 30 connections should pass without a limit")
		}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
	})

	It("isolates clients from each other", func() {
		ctx, cancel := contextWithTimeout(360 * time.Second)
		DeferCleanup(cancel)

		addEUSource(ctx)

		By("creating a service with rate=1/s burst=2")
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, "rl-isolation", 1)).To(Succeed())
		DeferCleanup(harness.TearDownBackend, ctx, sharedK8s(), e2eNamespace, "rl-isolation")
		Expect(harness.WaitForBackendReady(ctx, sharedK8s(), e2eNamespace, "rl-isolation", 90*time.Second)).To(Succeed())
		Expect(harness.CreateLBServiceWithRateLimit(ctx, sharedK8s(), e2eNamespace, "rl-isolation", rlHTTPPort, rlBackendPort, 1, 2)).To(Succeed())
		DeferCleanup(harness.DeleteService, ctx, sharedK8s(), e2eNamespace, "rl-isolation")
		Expect(harness.WaitForServiceFinalizer(ctx, sharedK8s(), e2eNamespace, "rl-isolation", 30*time.Second)).To(Succeed())
		ip, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, "rl-isolation", 60*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(ip).NotTo(BeEmpty())
		addr := ip + ":80"
		key := serviceKey("rl-isolation")
		waitForRateLimit(key, 1, 2)

		By("exhausting the US client's bucket")
		succeeded := 0
		Eventually(func(g Gomega) {
			var err error
			succeeded, err = harness.AttemptParallelFromNS(ctx, vepaClientNS, usSourceIP, addr, 20, 900*time.Millisecond)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(succeeded).To(BeNumerically("<=", 5))
		}).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		By("asserting a different client still has its own bucket")
		// The EU client starts with a full bucket (burst=2) and refills at
		// 1/s. A shared bucket exhausted by the US flood would leave it with
		// ~0 successes, so seeing at least its own burst proves isolation.
		Eventually(func(g Gomega) {
			n, err := harness.AttemptParallelFromNS(ctx, vepaClientNS, euSourceIP, addr, 5, 2*time.Second)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(n).To(BeNumerically(">=", 2),
				"the EU client must not be throttled by the US client's exhausted bucket")
		}).WithTimeout(20 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
	})
})
