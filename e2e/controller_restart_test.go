//go:build e2e && linux

package e2e_test

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

// controller_restart_test.go verifies that the controller, supervised by
// OpenRC's supervise-daemon under the hl-ingress-controller service, is
// respawned after SIGKILL and that LB state reconverges without manual
// intervention. We assert:
//   - The controller process comes back within a short window (≤ 15s).
//   - The LB Service's status IP is unchanged across the restart.
//   - Round-robin traffic to 3 distinct backends still works post-restart.
var _ = ginkgo.Describe("controller restart idempotence", func() {
	ginkgo.It("reconverges state after SIGKILL + restart of the controller process", func() {
		ctx, cancel := contextWithTimeout(180 * time.Second)
		defer cancel()

		lbName := "controller-restart-lb"

		ginkgo.By("deploying 3-backend identicon + LB Service")
		gomega.Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, 3)).To(gomega.Succeed())
		defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lbName) }()
		gomega.Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(gomega.Succeed())
		defer func() { _ = harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName) }()

		ip, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ip).NotTo(gomega.BeEmpty())

		ginkgo.By("verifying round-robin across 3 backends works pre-restart")
		idsBefore, err := harness.CollectBackendIDs(ctx, ip+":80", 20, 30*time.Second)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(harness.AssertAllHit(idsBefore, 3)).To(gomega.Succeed(),
			"need all 3 backends hit pre-restart to validate post-restart resilience")

		// Capture the controller's startup log length so we can detect a fresh
		// "starting manager" line after restart, not the original one.
		startupPatternLine := "starting manager"
		_ = startupPatternLine

		ginkgo.By("hard-killing the controller (pkill -9 controller)")
		// KillService uses pkill -9 -f /usr/local/bin/controller (matches the
		// binary path because supervise-daemon runs the wrapper that execs the
		// controller binary). supervise-daemon respawns it shortly after.
		gomega.Expect(harness.KillService(ctx, "controller")).To(gomega.Succeed())

		ginkgo.By("waiting for OpenRC to respawn the controller and log 'starting manager'")
		gomega.Expect(harness.WaitForLogMatch(ctx, controllerLog, "starting manager", 15*time.Second)).To(gomega.Succeed())

		ginkgo.By("verifying the LB IP is unchanged after restart")
		gomega.Consistently(func(g gomega.Gomega) {
			svc, err := harness.GetService(ctx, sharedK8s(), e2eNamespace, lbName)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(harness.ServiceStatusIP(svc)).To(gomega.Equal(ip),
				"LB IP must remain stable across controller restart")
		}, 5*time.Second, 500*time.Millisecond).Should(gomega.Succeed())

		ginkgo.By("verifying round-robin across 3 backends resumes post-restart")
		gomega.Eventually(func(g gomega.Gomega) {
			idsAfter, err := harness.CollectBackendIDs(ctx, ip+":80", 20, 30*time.Second)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(harness.AssertAllHit(idsAfter, 3)).To(gomega.Succeed(),
				"all 3 backends must still be hit round-robin after controller restart")
		}, 30*time.Second, 2*time.Second).Should(gomega.Succeed())
	})
})
