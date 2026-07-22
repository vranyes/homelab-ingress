//go:build e2e && linux

package e2e_test

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

// oidc_reject_test.go verifies that when the controller's bearer token is
// mutated to one bearing an unsupported audience, the ingress server rejects
// subsequent RPCs with codes.Unauthenticated. IsRetryable does not include
// Unauthenticated, so the controller's reconciler should surface the error
// in its log rather than silently requeue.
//
// Note: the token is re-read from disk on every gRPC RPC by the controller's
// `tokenFileCredentials.GetRequestMetadata`, so mutating the file is the
// natural mechanism to drive this test without restarting the controller.
var _ = ginkgo.Describe("oidc rejection", func() {
	ginkgo.It("rejects a wrong-audience token with Unauthenticated", func() {
		ctx, cancel := contextWithTimeout(120 * time.Second)
		defer cancel()

		// Save the original (good) token so we can restore it for the rest of
		// the suite. AfterAll/BeforeAll typically handles state, but this
		// scenario must NOT leak the bad token to subsequent scenarios.
		good, err := harness.ReadFile(tokenPath)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			_ = harness.MutateToken(tokenPath, string(good))
			// Give the controller a moment to pick up the restored token via
			// its next RPC.
			time.Sleep(2 * time.Second)
		}()

		ginkgo.By("minting a token with aud=bogus and writing it to " + tokenPath)
		bad, err := harness.BadAudienceToken(ctx, oidcBase, controllerSub)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(harness.MutateToken(tokenPath, bad)).To(gomega.Succeed())

		ginkgo.By("triggering a controller reconcile by creating a trivial LB Service")
		lbName := "oidc-reject"
		gomega.Expect(harness.EnsureNamespace(ctx, sharedK8s(), e2eNamespace)).To(gomega.Succeed())
		gomega.Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(gomega.Succeed())
		defer func() { _ = harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName) }()

		// Unauthenticated is not in IsRetryable, so the controller logs an
		// error containing "get from router" or "create on router" with the
		// gRPC status description. Look for either substring.
		ginkgo.By("waiting for the controller log to surface an Unauthenticated error")
		// Status text in gRPC errors contains "Unauthenticated" when the
		// interceptor rejects the token (see ingress/internal/auth/auth.go).
		gomega.Eventually(func(g gomega.Gomega) {
			count, err := harness.GrepCountFile(controllerLog, "Unauthenticated")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(count).To(gomega.BeNumerically(">", 0),
				"expected controller log to surface Unauthenticated after bad token mutation")
		}, 30*time.Second, 500*time.Millisecond).Should(gomega.Succeed())

		ginkgo.By("verifying that the Service did NOT receive a status IP (reconciler stalled)")
		gomega.Consistently(func(g gomega.Gomega) {
			svc, err := harness.GetService(ctx, sharedK8s(), e2eNamespace, lbName)
			if err != nil {
				return
			}
			g.Expect(harness.ServiceStatusIP(svc)).To(gomega.BeEmpty(),
				"controller should not have been able to patch status with a bad token")
		}, 5*time.Second, 500*time.Millisecond).Should(gomega.Succeed())
	})
})
