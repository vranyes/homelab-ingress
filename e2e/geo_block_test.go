//go:build e2e && linux

package e2e_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

// Geo-blocking scenarios. The mock geo fixture carves the e2e bridge
// subnet into four /26 slices:
//
//	10.44.0.0/26    US  (contains hl-br0 and ns-server's primary address)
//	10.44.0.64/26   CA
//	10.44.0.128/26  EU  (ns-server's secondary source address)
//	10.44.0.192/26  XX
//
// Traffic is always sent from ns-server so it traverses the daemon's
// prerouting chains. To exercise a different country we add the EU address to
// veth-outer and bind curl to it with --interface.
const (
	vepaOuterDev   = "veth-outer"
	usSourceIP     = "10.44.0.2" // ns-server's primary address
	euSourceIP     = "10.44.0.129"
	euSourceCIDR   = "10.44.0.129/24"
	geoHTTPPort    = 80
	geoBackendPort = 8080
)

func geoServiceKey(name string) string { return e2eNamespace + "/" + name }

// setupGeoLB deploys one backend and a LoadBalancer Service carrying the given
// allowed-geos annotation, then waits for its status IP.
func setupGeoLB(ctx context.Context, name string, geos []string) string {
	By("deploying backend for " + name)
	Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, name, 1)).To(Succeed())
	DeferCleanup(harness.TearDownBackend, ctx, sharedK8s(), e2eNamespace, name)
	Expect(harness.WaitForBackendReady(ctx, sharedK8s(), e2eNamespace, name, 90*time.Second)).To(Succeed())

	By(fmt.Sprintf("creating LoadBalancer Service %s with allowed_geos=%v", name, geos))
	Expect(harness.CreateLBServiceWithGeos(ctx, sharedK8s(), e2eNamespace, name, geoHTTPPort, geoBackendPort, geos)).To(Succeed())
	DeferCleanup(harness.DeleteService, ctx, sharedK8s(), e2eNamespace, name)

	Expect(harness.WaitForServiceFinalizer(ctx, sharedK8s(), e2eNamespace, name, 30*time.Second)).To(Succeed())
	ip, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, name, 60*time.Second)
	Expect(err).NotTo(HaveOccurred())
	Expect(ip).NotTo(BeEmpty())
	return ip
}

// addEUSource adds the EU-mapped secondary address to ns-server and removes it
// again when the spec finishes.
func addEUSource(ctx context.Context) {
	By("adding the EU source address to " + vepaClientNS)
	Expect(harness.AddNSAddress(ctx, vepaClientNS, vepaOuterDev, euSourceCIDR)).To(Succeed())
	DeferCleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = harness.RemoveNSAddress(cleanupCtx, vepaClientNS, vepaOuterDev, euSourceCIDR)
	})
}

// eventuallyBlocked asserts that traffic to lbAddr from sourceIP is dropped.
func eventuallyBlocked(ctx context.Context, sourceIP, lbAddr string) {
	Eventually(func(g Gomega) {
		_, allowed, _ := harness.AttemptRequestsFromNS(ctx, vepaClientNS, sourceIP, lbAddr, 3, time.Second)
		g.Expect(allowed).To(BeZero(), "geo-blocked traffic was allowed through")
	}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
}

// eventuallyAllowed asserts that traffic to lbAddr from sourceIP reaches the
// backend.
func eventuallyAllowed(ctx context.Context, sourceIP, lbAddr string) {
	Eventually(func(g Gomega) {
		ids, err := harness.CollectBackendIDsFromNSSource(ctx, vepaClientNS, sourceIP, lbAddr, 2, 15*time.Second)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ids).To(HaveLen(2))
	}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
}

var _ = Describe("geo-blocking", func() {
	It("installs the geo deny before DNAT and creates a per-service union set", func() {
		ctx, cancel := contextWithTimeout(240 * time.Second)
		DeferCleanup(cancel)

		geoLB := setupGeoLB(ctx, "geo-rules", []string{"US"})
		key := geoServiceKey("geo-rules")

		By("asserting rule order: geo deny, DNAT, final drop")
		Eventually(func(g Gomega) {
			rules, err := harness.ClassifyRules(key)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(rules).NotTo(BeEmpty())
			g.Expect(rules[0].Kind).To(Equal(harness.RuleKindGeoDeny),
				"first rule must be the geo deny (ADR 0001)")
			g.Expect(rules[len(rules)-1].Kind).To(Equal(harness.RuleKindDrop),
				"last rule must be the daddr drop")
			g.Expect(harness.CountRuleKind(rules, harness.RuleKindGeoDeny)).To(Equal(1))
			g.Expect(harness.CountRuleKind(rules, harness.RuleKindDNAT)).To(Equal(1))
		}).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		By("asserting the union set exists and is populated")
		Eventually(func(g Gomega) {
			g.Expect(harness.AssertGeoSetExists(key)).To(Succeed())
		}).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		// The US range is /26, which is two interval elements (start + end).
		_, count, err := harness.GetGeoSet(key)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeNumerically(">=", 2))

		By("asserting US traffic still reaches the backend")
		eventuallyAllowed(ctx, usSourceIP, geoLB+":80")
	})

	It("allows US sources and blocks EU sources, then follows an annotation change", func() {
		ctx, cancel := contextWithTimeout(360 * time.Second)
		DeferCleanup(cancel)

		addEUSource(ctx)
		geoLB := setupGeoLB(ctx, "geo-us", []string{"US"})
		addr := geoLB + ":80"
		key := geoServiceKey("geo-us")

		By("confirming the US-mapped source is allowed")
		eventuallyAllowed(ctx, usSourceIP, addr)

		By("asserting the EU-mapped source is dropped")
		before, ok := harness.CounterFor(mustClassify(key), harness.RuleKindGeoDeny)
		Expect(ok).To(BeTrue(), "geo deny rule missing")
		eventuallyBlocked(ctx, euSourceIP, addr)

		By("asserting the geo deny counter advanced")
		Eventually(func(g Gomega) {
			after, ok := harness.CounterFor(mustClassify(key), harness.RuleKindGeoDeny)
			g.Expect(ok).To(BeTrue())
			g.Expect(after.Packets).To(BeNumerically(">", before.Packets))
		}).WithTimeout(15 * time.Second).WithPolling(time.Second).Should(Succeed())

		By("flipping the allow-list to EU via the Service annotation")
		Expect(harness.SetAllowedGeos(ctx, sharedK8s(), e2eNamespace, "geo-us", []string{"EU"})).To(Succeed())

		By("asserting EU now reaches the backend and US is blocked")
		eventuallyAllowed(ctx, euSourceIP, addr)
		eventuallyBlocked(ctx, usSourceIP, addr)

		By("restoring the original allow-list")
		Expect(harness.SetAllowedGeos(ctx, sharedK8s(), e2eNamespace, "geo-us", []string{"US"})).To(Succeed())
		eventuallyAllowed(ctx, usSourceIP, addr)
	})

	It("reloads the database when the geo file changes", func() {
		ctx, cancel := contextWithTimeout(300 * time.Second)
		DeferCleanup(cancel)

		geoLB := setupGeoLB(ctx, "geo-reload", []string{"US"})
		addr := geoLB + ":80"

		By("confirming US traffic works with the canonical fixture")
		eventuallyAllowed(ctx, usSourceIP, addr)

		original, err := harness.ReadFile(harness.MockGeoPath)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			_ = harness.WriteFileAtomic(harness.MockGeoPath, original, 0o644)
			// Give the watcher a chance to restore the original mapping before
			// the next spec starts.
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				ids, err := harness.CollectBackendIDsFromNS(cleanupCtx, vepaClientNS, addr, 1, 10*time.Second)
				if err == nil && len(ids) == 1 {
					return
				}
				time.Sleep(time.Second)
			}
		})

		By("swapping the US and EU ranges in the fixture")
		swapped := strings.Replace(string(original), "10.44.0.0/26\tUS", "10.44.0.0/26\tEU", 1)
		swapped = strings.Replace(swapped, "10.44.0.128/26\tEU", "10.44.0.128/26\tUS", 1)
		Expect(swapped).NotTo(Equal(string(original)))
		Expect(harness.WriteFileAtomic(harness.MockGeoPath, []byte(swapped), 0o644)).To(Succeed())

		By("asserting the same US source is now blocked")
		eventuallyBlocked(ctx, usSourceIP, addr)
	})

	It("keeps per-service allow-lists independent", func() {
		ctx, cancel := contextWithTimeout(360 * time.Second)
		DeferCleanup(cancel)

		addEUSource(ctx)
		usLB := setupGeoLB(ctx, "geo-split-us", []string{"US"})
		euLB := setupGeoLB(ctx, "geo-split-eu", []string{"EU"})

		By("asserting the US source reaches only the US service")
		eventuallyAllowed(ctx, usSourceIP, usLB+":80")
		eventuallyBlocked(ctx, usSourceIP, euLB+":80")

		By("asserting the EU source reaches only the EU service")
		eventuallyAllowed(ctx, euSourceIP, euLB+":80")
		eventuallyBlocked(ctx, euSourceIP, usLB+":80")

		By("asserting both services have populated union sets")
		Expect(harness.AssertGeoSetExists(geoServiceKey("geo-split-us"))).To(Succeed())
		Expect(harness.AssertGeoSetExists(geoServiceKey("geo-split-eu"))).To(Succeed())
	})
})

// mustClassify returns the classified rules or fails the spec.
func mustClassify(serviceKey string) []harness.RuleInfo {
	rules, err := harness.ClassifyRules(serviceKey)
	Expect(err).NotTo(HaveOccurred())
	return rules
}
