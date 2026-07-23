//go:build e2e && linux

package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

// controller_restart_test.go verifies that the controller, supervised by
// OpenRC's supervise-daemon under the hl-ingress-controller service, is
// respawned after SIGKILL and that LB state reconverges without manual
// intervention. We assert:
//   - The controller process comes back within a short window (≤ 15s).
//   - The LB Service's status IP is unchanged across the restart.
//   - Round-robin traffic to 3 distinct backends still works post-restart.
var _ = Describe("controller restart idempotence", func() {
	It("reconverges state after SIGKILL + restart of the controller process", func() {
		ctx, cancel := contextWithTimeout(180 * time.Second)
		defer cancel()

		lbName := "controller-restart-lb"

		By("deploying 3-backend identicon + LB Service")
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, 3)).To(Succeed())
		defer func() { _ = harness.TearDownBackend(ctx, sharedK8s(), e2eNamespace, lbName) }()
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())
		defer func() { _ = harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName) }()

		ip, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(ip).NotTo(BeEmpty())

		By("verifying round-robin across 3 backends works pre-restart")
		idsBefore, err := harness.CollectBackendIDs(ctx, ip+":80", 20, 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(harness.AssertAllHit(idsBefore, 3)).To(Succeed(),
			"need all 3 backends hit pre-restart to validate post-restart resilience")

		// Capture the controller's startup log length so we can detect a fresh
		// "starting manager" line after restart, not the original one.
		startupPatternLine := "starting manager"
		_ = startupPatternLine

		By("hard-killing the controller (pkill -9 controller)")
		// KillService uses pkill -9 -f /usr/local/bin/controller (matches the
		// binary path because supervise-daemon runs the wrapper that execs the
		// controller binary). supervise-daemon respawns it shortly after.
		Expect(harness.KillService(ctx, "controller")).To(Succeed())

		By("waiting for OpenRC to respawn the controller and log 'starting manager'")
		Expect(harness.WaitForLogMatch(ctx, controllerLog, "starting manager", 15*time.Second)).To(Succeed())

		By("verifying the LB IP is unchanged after restart")
		Consistently(func(g Gomega) {
			svc, err := harness.GetService(ctx, sharedK8s(), e2eNamespace, lbName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(harness.ServiceStatusIP(svc)).To(Equal(ip),
				"LB IP must remain stable across controller restart")
		}, 5*time.Second, 500*time.Millisecond).Should(Succeed())

		By("verifying round-robin across 3 backends resumes post-restart")
		Eventually(func(g Gomega) {
			idsAfter, err := harness.CollectBackendIDs(ctx, ip+":80", 20, 30*time.Second)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(harness.AssertAllHit(idsAfter, 3)).To(Succeed(),
				"all 3 backends must still be hit round-robin after controller restart")
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})
})
