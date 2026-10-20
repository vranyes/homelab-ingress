//go:build e2e && linux

package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

var _ = Describe("zero-lb baseline", func() {
	It("reports zero active macvlans and no per-service nftables chains", func() {
		ctx, cancel := contextWithTimeout(30 * time.Second)
		defer cancel()

		Eventually(func(g Gomega) {
			health, err := sharedRouter().Health(ctx, healthReq())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(health.GetActiveMacvlans()).To(BeZero(), "expected zero active macvlans before any LB is created")
		}, 10*time.Second, 500*time.Millisecond).Should(Succeed())

		Expect(harness.AssertNoLBChains()).To(Succeed())
		// The "no ERROR lines at startup" assertion lives in BeforeSuite: it
		// must run before any spec can produce controller errors, and this spec
		// runs in randomized order.
	})
})
