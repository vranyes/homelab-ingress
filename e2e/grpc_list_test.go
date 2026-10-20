//go:build e2e && linux

package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

// grpc_list_test.go pins Get/List agreement at the e2e level. Regression
// coverage for the "<nil>" bug: ListLoadBalancers used to return
// assigned_ip "<nil>" (net.IP(nil).String()) for DHCP-pending services,
// which the controller's IP poller then wrote into Service status as a
// literal IP. The unit test in ingress/internal/service pins the server
// behavior; this spec pins the end-to-end contract both RPCs must honor:
// after convergence, Get and List report the same non-empty IP, and List
// never contains "<nil>".
var _ = Describe("grpc list consistency", func() {
	It("reports the same assigned IP via Get and List", func() {
		ctx, cancel := contextWithTimeout(300 * time.Second)
		DeferCleanup(cancel)

		lbName := "grpc-list"
		key := serviceKey(lbName)

		By("deploying 1 backend + LB Service")
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, 1)).To(Succeed())
		DeferCleanup(harness.TearDownBackend, ctx, sharedK8s(), e2eNamespace, lbName)
		By("waiting for backend pods to be Ready (fails fast on ImagePullBackOff)")
		Expect(harness.WaitForBackendReady(ctx, sharedK8s(), e2eNamespace, lbName, 90*time.Second)).To(Succeed())
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())
		DeferCleanup(harness.DeleteService, ctx, sharedK8s(), e2eNamespace, lbName)

		By("waiting for the LoadBalancer Service to receive a status IP")
		lbIP, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(lbIP).NotTo(BeEmpty())
		Expect(lbIP).NotTo(Equal("<nil>"), "status IP must never be the literal string <nil>")

		By("asserting Get and List agree on the assigned IP")
		Eventually(func(g Gomega) {
			get, err := sharedRouter().GetLoadBalancer(ctx, getLBReq(key))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(get.GetAssignedIp()).To(Equal(lbIP))

			list, err := sharedRouter().ListLoadBalancers(ctx, listLBReq())
			g.Expect(err).NotTo(HaveOccurred())
			var found string
			for _, lb := range list.LoadBalancers {
				g.Expect(lb.GetAssignedIp()).NotTo(Equal("<nil>"),
					"List must never report assigned_ip <nil>, even for DHCP-pending LBs")
				if lb.GetServiceKey() == key {
					found = lb.GetAssignedIp()
				}
			}
			g.Expect(found).To(Equal(lbIP), "List must contain this LB with the converged IP")
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})
})
