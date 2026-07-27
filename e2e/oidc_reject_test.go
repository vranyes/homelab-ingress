//go:build e2e && linux

package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
var _ = Describe("oidc rejection", func() {
	It("rejects a wrong-audience token with Unauthenticated", func() {
		ctx, cancel := contextWithTimeout(120 * time.Second)
		DeferCleanup(cancel)

		// Save the original (good) token so we can restore it for the rest of
		// the suite. AfterAll/BeforeAll typically handles state, but this
		// scenario must NOT leak the bad token to subsequent scenarios.
		good, err := harness.ReadFile(tokenPath)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(harness.MutateToken, tokenPath, string(good))

		By("minting a token with aud=bogus and writing it to " + tokenPath)
		bad, err := harness.BadAudienceToken(ctx, oidcBase, controllerSub)
		Expect(err).NotTo(HaveOccurred())
		Expect(harness.MutateToken(tokenPath, bad)).To(Succeed())

		By("triggering a controller reconcile by creating a trivial LB Service")
		lbName := "oidc-reject"
		Eventually(harness.EnsureNamespace).
			WithTimeout(5*time.Second).
			WithArguments(ctx, sharedK8s(), e2eNamespace).
			Should(Succeed())
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())
		DeferCleanup(harness.DeleteService, sharedK8s(), e2eNamespace, lbName)

		// Unauthenticated is not in IsRetryable, so the controller logs an
		// error containing "get from router" or "create on router" with the
		// gRPC status description. Look for either substring.
		By("waiting for the controller log to surface an Unauthenticated error")
		// Status text in gRPC errors contains "Unauthenticated" when the
		// interceptor rejects the token (see ingress/internal/auth/auth.go).
		Eventually(harness.GrepCountFile).
			WithTimeout(30*time.Second).
			WithArguments(controllerLog, "Unauthenticated").
			Should(BeNumerically(">", 0), "expected controller log to surface Unauthenticated after bad token mutation")

		By("verifying that the Service did NOT receive a status IP (reconciler stalled)")
		Consistently(func(g Gomega) {
			svc, err := harness.GetService(ctx, sharedK8s(), e2eNamespace, lbName)
			if err != nil {
				return
			}
			g.Expect(harness.ServiceStatusIP(svc)).To(BeEmpty(),
				"controller should not have been able to patch status with a bad token")
		}, 5*time.Second, 500*time.Millisecond).Should(Succeed())
	})
})
