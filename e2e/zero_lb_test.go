//go:build e2e && linux

package e2e_test

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

var _ = ginkgo.Describe("zero-lb baseline", func() {
	ginkgo.It("reports zero active macvlans and no per-service nftables chains", func() {
		ctx, cancel := contextWithTimeout(30 * time.Second)
		defer cancel()

		gomega.Eventually(func(g gomega.Gomega) {
			health, err := sharedRouter().Health(ctx, healthReq())
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(health.GetActiveMacvlans()).To(gomega.BeZero(), "expected zero active macvlans before any LB is created")
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Succeed())

		gomega.Expect(harness.AssertNoLBChains()).To(gomega.Succeed())

		errCount, err := harness.GrepCountFile(controllerLog, "ERROR")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(errCount).To(gomega.BeZero(), "expected no ERROR log lines in a clean zero-LB baseline")
	})
})
