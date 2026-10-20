//go:build e2e && linux

package e2e_test

import (
	"context"
	"fmt"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"

	"github.com/vranyes/homelab-ingress/e2e/harness"
)

var _ = Describe("two-lb-masq counter validation", func() {
	It("should round-robin across 2 backends with zero drop-rule hits and masquerade", func() {
		lbName := "two-lb-masq"
		svcKey := e2eNamespace + "/" + lbName
		ctx, cancel := contextWithTimeout(300 * time.Second)
		DeferCleanup(cancel)

		By("deploying 2 identicon backends for " + lbName)
		Expect(harness.DeployBackend(ctx, sharedK8s(), e2eNamespace, lbName, 2)).To(Succeed())
		DeferCleanup(harness.TearDownBackend, ctx, sharedK8s(), e2eNamespace, lbName)
		By("waiting for backend pods to be Ready (fails fast on ImagePullBackOff)")
		Expect(harness.WaitForBackendReady(ctx, sharedK8s(), e2eNamespace, lbName, 90*time.Second)).To(Succeed())

		By("creating LoadBalancer Service " + lbName)
		Expect(harness.CreateLBService(ctx, sharedK8s(), e2eNamespace, lbName, 80, 8080)).To(Succeed())
		DeferCleanup(harness.DeleteService, ctx, sharedK8s(), e2eNamespace, lbName)

		By("waiting for the homelab-ingress finalizer")
		Expect(harness.WaitForServiceFinalizer(ctx, sharedK8s(), e2eNamespace, lbName, 30*time.Second)).To(Succeed())

		By("waiting for LB status IP")
		lbIP, err := harness.WaitForServiceStatusIP(ctx, sharedK8s(), e2eNamespace, lbName, 60*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(lbIP).NotTo(BeEmpty())

		chainName := harness.LBChainName(svcKey)

		By("waiting for nftables chain " + chainName + " to exist")
		Eventually(func(g Gomega) {
			g.Expect(harness.AssertLBChainExists(svcKey)).To(Succeed())
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

		By("adding postrouting masquerade rule to match production topology")
		masqRuleHandle := addPostroutingMasquerade(ctx)
		DeferCleanup(removePostroutingRule, masqRuleHandle)

		By("snapshotting pre-test counters for chain " + chainName)
		var before map[uint64]harness.CounterStat
		Eventually(func(g Gomega) {
			var err error
			before, err = harness.GetRuleCounters(harness.NFTTableName, chainName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(before).NotTo(BeEmpty())
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

		By(fmt.Sprintf("firing 50 HTTP requests (fresh connections) against %s:80", lbIP))
		var ids []string
		Eventually(func(g Gomega) {
			var err error
			ids, err = collectIDs(ctx, lbIP+":80", 50, 60*time.Second)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ids).To(HaveLen(50), "expected all 50 requests to hit a backend")
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		By("snapshotting post-test counters")
		after, err := harness.GetRuleCounters(harness.NFTTableName, chainName)
		Expect(err).NotTo(HaveOccurred())

		By("computing and logging counter deltas")
		handles := make([]uint64, 0, len(after))
		for h := range after {
			handles = append(handles, h)
		}
		sort.Slice(handles, func(i, j int) bool { return handles[i] < handles[j] })

		fmt.Fprintf(GinkgoWriter, "rule handles (sorted): %v\n", handles)
		for _, h := range handles {
			b := before[h]
			a := after[h]
			fmt.Fprintf(GinkgoWriter,
				"  handle %d: before={pkt=%d, bytes=%d} after={pkt=%d, bytes=%d} delta={pkt=%d, bytes=%d}\n",
				h, b.Packets, b.Bytes, a.Packets, a.Bytes,
				a.Packets-b.Packets, a.Bytes-b.Bytes)
		}

		By("asserting the DNAT rule counter advanced for the test traffic")
		dnatDelta := after[handles[0]].Packets - before[handles[0]].Packets
		Expect(dnatDelta).To(BeNumerically(">=", 50),
			"expected the DNAT rule counter to count the 50 requests; a zero delta means traffic never traversed the ingress chain (e.g. it was handled by kube-proxy or dropped earlier)")

		By("asserting drop-rule counter is zero")
		dropHandle := handles[len(handles)-1]
		dropDelta := after[dropHandle].Packets - before[dropHandle].Packets
		fmt.Fprintf(GinkgoWriter, "drop rule handle=%d packet delta=%d\n", dropHandle, dropDelta)
		Expect(dropDelta).To(BeZero(),
			"expected zero drop-rule hits; non-zero means packets are being silently dropped")

		By("asserting both backends were reached with roughly uniform distribution")
		Expect(harness.AssertAllHit(ids, 2)).To(Succeed())
		Expect(harness.AssertRoughlyUniform(ids, 0.25)).To(Succeed())
	})
})

// addPostroutingMasquerade adds a masquerade rule to the postrouting chain:
//
//	iifname "hl-br0" oifname "hl-br0" ct status dnat masquerade
//
// hl-br0 is the VM's equivalent of the production br-lan pair: the bridge
// carrying both the LB IPs and the test client, so hairpinned (same-bridge
// in/out) DNAT traffic matches. A previous revision used the production
// interface names (br-lan.10), which do not exist in the VM — nftables
// accepted the rule but it could never match, making the test vacuous.
// Returns the rule handle for later deletion.
func addPostroutingMasquerade(ctx context.Context) uint64 {
	conn, err := nftables.New()
	Expect(err).NotTo(HaveOccurred())

	tables, err := conn.ListTables()
	Expect(err).NotTo(HaveOccurred())

	var table *nftables.Table
	for _, t := range tables {
		if t.Name == harness.NFTTableName {
			table = t
			break
		}
	}
	Expect(table).NotTo(BeNil(), "table %s not found", harness.NFTTableName)

	chains, err := conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	Expect(err).NotTo(HaveOccurred())

	var postChain *nftables.Chain
	for _, c := range chains {
		if c.Name == "postrouting" && c.Table != nil && c.Table.Name == harness.NFTTableName {
			postChain = c
			break
		}
	}
	Expect(postChain).NotTo(BeNil(), "postrouting chain not found")

	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: postChain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte("hl-br0\x00")},
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte("hl-br0\x00")},
			&expr.Ct{Key: expr.CtKeySTATUS, Register: 1},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            4,
				Mask:           binaryutil.NativeEndian.PutUint32(1 << 5),
				Xor:            binaryutil.NativeEndian.PutUint32(0),
			},
			&expr.Cmp{
				Op:       expr.CmpOpNeq,
				Register: 1,
				Data:     binaryutil.NativeEndian.PutUint32(0),
			},
			&expr.Masq{},
		},
	})
	Expect(conn.Flush()).To(Succeed())

	rules, err := conn.GetRules(table, postChain)
	Expect(err).NotTo(HaveOccurred())
	Expect(rules).To(HaveLen(1), "expected exactly 1 rule in postrouting chain")
	return rules[0].Handle
}

// removePostroutingRule deletes an nftables rule by handle from the postrouting
// chain. Best effort — ignores errors.
func removePostroutingRule(handle uint64) {
	if handle == 0 {
		return
	}
	conn, err := nftables.New()
	if err != nil {
		return
	}

	tables, err := conn.ListTables()
	if err != nil {
		return
	}
	var table *nftables.Table
	for _, t := range tables {
		if t.Name == harness.NFTTableName {
			table = t
			break
		}
	}
	if table == nil {
		return
	}
	chains, err := conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return
	}
	var postChain *nftables.Chain
	for _, c := range chains {
		if c.Name == "postrouting" && c.Table != nil && c.Table.Name == harness.NFTTableName {
			postChain = c
			break
		}
	}
	if postChain == nil {
		return
	}

	conn.DelRule(&nftables.Rule{
		Table:  table,
		Chain:  postChain,
		Handle: handle,
	})
	_ = conn.Flush()
}
