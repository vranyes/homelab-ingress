//go:build e2e && linux

package e2e_test

import (
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

// ingressLog mirrors the path in suite_test.go's BeforeSuite Spawn call.
// Log greps below are best-effort corroboration only; status + traffic are
// the binding assertions.
const ingressLog = "/var/log/hl-ingress/ingress.log"

// dhcp_lease_test.go is the black-box half of DHCP coverage: it drives the
// real dnsmasq exchange through the same harness flow as the other specs
// (backend -> LB Service -> status IP -> traffic -> delete) and asserts only
// what is observable without ingress internals: the Service status IP, live
// traffic, and cleanup. It imports no ingress/internal packages.
//
// Explicitly out of scope here (covered by unit tests instead):
//   - Renewal timers: dnsmasq leases are 24h (T1 ~12h), unobservable in a
//     40m suite. See ingress/internal/dhcp for Manager-level tests.
//   - Daemon restart / lease persist-restore: BeforeSuite spawns one shared
//     ingress daemon; restarting it mid-suite would orphan other specs'
//     LBs. Covered by TestManagerPersistRestoreAcrossNewManager.
var _ = Describe("dhcp black-box", func() {
	It("acquires an ingress IP from dnsmasq and routes traffic to it", func() {
		ctx, cancel := contextWithTimeout(300 * time.Second)
		DeferCleanup(cancel)

		lbName := "dhcp-lease"

		By("deploying 1 backend + LB Service")
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, 1)).To(Succeed())
		DeferCleanup(harness.TearDownBackend, ctx, sharedK8s(), e2eNamespace, lbName)
		By("waiting for backend pods to be Ready (fails fast on ImagePullBackOff)")
		Expect(harness.WaitForBackendReady(ctx, sharedK8s(), e2eNamespace, lbName, 90*time.Second)).To(Succeed())
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())
		DeferCleanup(harness.DeleteService, ctx, sharedK8s(), e2eNamespace, lbName)

		By("waiting for the DHCP-acquired status IP")
		lbIP, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 90*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(lbIP).NotTo(BeEmpty())
		assertLeasePoolIP(lbIP)

		By("verifying traffic flows to the acquired IP (fresh connections)")
		Eventually(func(g Gomega) {
			ids, err := collectIDs(ctx, lbIP+":80", 20, 30*time.Second)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ids).To(HaveLen(20), "expected all 20 requests to hit a backend")
			g.Expect(harness.AssertAllHit(ids, 1)).To(Succeed())
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Best-effort corroboration only: the daemon logs the state update
		// that the Manager performs on acquire. Traffic + status above are
		// the binding assertions; log wording may evolve.
		By("noting the acquire log line (best effort, non-binding)")
		if line, err := harness.GrepFile(ingressLog, "DHCP IP change"); err != nil || line == "" {
			GinkgoWriter.Printf("note: no 'DHCP IP change' line in controller log (non-binding check)\n")
		}
	})

	It("releases the lease on Service delete: status clears, chain is gone", func() {
		ctx, cancel := contextWithTimeout(300 * time.Second)
		DeferCleanup(cancel)

		lbName := "dhcp-release"

		By("deploying 1 backend + LB Service and waiting for the status IP")
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, 1)).To(Succeed())
		DeferCleanup(harness.TearDownBackend, ctx, sharedK8s(), e2eNamespace, lbName)
		By("waiting for backend pods to be Ready (fails fast on ImagePullBackOff)")
		Expect(harness.WaitForBackendReady(ctx, sharedK8s(), e2eNamespace, lbName, 90*time.Second)).To(Succeed())
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())

		lbIP, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 90*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(lbIP).NotTo(BeEmpty())

		By("deleting the LB Service")
		Expect(harness.DeleteService(ctx, sharedK8s(), e2eNamespace, lbName)).To(Succeed())

		By("waiting for the finalizer to be removed")
		Expect(harness.WaitForServiceNoFinalizer(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)).To(Succeed())

		By("asserting the per-service nftables chain is gone")
		Eventually(func(g Gomega) {
			g.Expect(harness.AssertNoLBChains()).To(Succeed(),
				"per-service nftables chains must be gone after delete")
		}, 30*time.Second, 500*time.Millisecond).Should(Succeed())

		// Best-effort corroboration only (see acquire spec).
		By("noting the client-removal log line (best effort, non-binding)")
		if line, err := harness.GrepFile(ingressLog, "removing DHCP client"); err != nil || line == "" {
			GinkgoWriter.Printf("note: no 'removing DHCP client' line in controller log (non-binding check)\n")
		}
	})
})

// assertLeasePoolIP asserts the status IP came from the dnsmasq pool
// (10.44.0.10-100/24 per e2e-vm/lima.yaml) rather than e.g. the bridge
// address (.1) or a "<nil>" placeholder from a List/Get mismatch.
func assertLeasePoolIP(lbIP string) {
	ExpectWithOffset(1, lbIP).NotTo(BeEmpty())
	ExpectWithOffset(1, lbIP).NotTo(Equal("<nil>"), "status IP must never be the literal string <nil>")
	ip := net.ParseIP(lbIP)
	ExpectWithOffset(1, ip).NotTo(BeNil(), "status IP %q must parse", lbIP)
	ExpectWithOffset(1, ip.To4()).NotTo(BeNil(), "status IP %q must be IPv4", lbIP)
	_, cidr, err := net.ParseCIDR("10.44.0.0/24")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, cidr.Contains(ip)).To(BeTrue(), "status IP %q must be on hl-br0", lbIP)
	last := ip.To4()[3]
	ExpectWithOffset(1, last >= 10 && last <= 100).To(BeTrue(),
		"status IP %q must fall in the dnsmasq pool 10.44.0.10-100", lbIP)
}
