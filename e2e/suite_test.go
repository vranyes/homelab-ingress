//go:build e2e && linux

package e2e_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	routerv1 "github.com/vranyes/homelab-ingress/controller/pkg/api/router/v1"
	"github.com/vranyes/homelab-ingress/e2e/harness"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Paths inside the lima VM. These mirror the layout produced by
// e2e-vm/lima.yaml.
const (
	e2eNamespace      = "hl-ingress-e2e"
	ingressAddr       = "127.0.0.1:50051"
	oidcBase          = "http://127.0.0.1:8080"
	caPEMPath         = "/var/lib/homelab-ingress/ca.pem"
	tokenPath         = "/var/run/hl-ingress/token"
	ingressConfigPath = "/etc/homelab-ingress/config.yaml"
	controllerLog     = "/var/log/hl-ingress/controller.log"

	// The controller's subject claim — the harness mints a JWT with this
	// sub and writes it to tokenPath before starting the controller. The
	// ingress auth.allows_subjects list in the config file must include
	// this value.
	controllerSub = "system:serviceaccount:default:homelab-ingress-controller"
	audience      = "homelab-ingress"
)

// vepaClientNS is the VEPA peer network namespace. Scenario traffic is sent
// from inside it so packets arrive over the bridge and traverse the ingress
// daemon's prerouting (DNAT) chains. Traffic originating in the host namespace
// is delivered locally and DNATed by kube-proxy in the OUTPUT chain, which
// means it never exercises the product's rules at all.
const vepaClientNS = "ns-server"

// collectIDs sends fresh-connection HTTP requests from the VEPA peer and
// returns the backend hostname seen by each response.
func collectIDs(ctx context.Context, addr string, n int, timeout time.Duration) ([]string, error) {
	return harness.CollectBackendIDsFromNS(ctx, vepaClientNS, addr, n, timeout)
}

// sharedContext is populated in BeforeSuite and torn down in AfterSuite.
// Scenarios read it via shared() rather than capturing the values in
// closure variables — that makes test ordering independent and avoids
// stale captures when the same scenarios re-run on developer shells.
var shared = struct {
	k8s            client.Client
	router         routerv1.RouterServiceClient
	routerConn     *grpc.ClientConn
	oidcProc       *harness.ManagedProcess
	ingressProc    *harness.ManagedProcess
	controllerProc *harness.ManagedProcess
}{}

func sharedK8s() client.Client                   { return shared.k8s }
func sharedRouter() routerv1.RouterServiceClient { return shared.router }

// TestE2E is the Ginkgo entry point. It is only compiled under -tags=e2e.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "homelab-ingress e2e suite")
}

var _ = BeforeSuite(func() {
	ctx := context.Background()

	// Allow up to 5 minutes for full suite setup; we override the suite-level
	// timeout indirectly by extending ctx via deadline in each step.
	_ = ctx

	By("resetting dnsmasq leases from any previous run")
	// Kept VMs accumulate leases when a run is interrupted; the pool is only
	// 91 addresses and a full pool makes every later spec time out waiting
	// for a status IP with no useful signal. Restarting also validates the
	// unit before the relay preflight below.
	Expect(harness.ResetDHCPLeases(ctx)).To(Succeed())

	By("verifying VEPA hairpin relay (ns-server + tc mirred + dnsmasq) is up")
	Expect(harness.EnsureVepaRelayUp(ctx)).To(Succeed(),
		"VEPA relay not healthy — re-run `make start-e2e-vm`; see diagnostic dump for details")

	By("creating state and log directories")
	_, err := harness.RunShell(ctx, "mkdir -p /var/log/hl-ingress /var/run/hl-ingress /var/lib/homelab-ingress /etc/homelab-ingress")
	Expect(err).NotTo(HaveOccurred())

	By("truncating service logs from any previous run")
	// Spawn opens logs with O_APPEND and assertions such as the zero-LB
	// baseline count ERROR lines in the whole file. Stale lines from an
	// earlier run (or a previous crashed controller) must not leak into this
	// one; `: >` also creates the file when it does not exist yet.
	_, err = harness.RunShell(ctx, "for f in ingress controller oidc; do : > /var/log/hl-ingress/$f.log; done")
	Expect(err).NotTo(HaveOccurred())

	By("resetting the mock geo database")
	// Specs mutate this fixture to prove the watcher reloads it. Always start
	// from the canonical carve-up, and write atomically so a crashed run can
	// never leave a half-written file behind.
	Expect(harness.WriteFileAtomic(harness.MockGeoPath, []byte(harness.DefaultGeoFixture()), 0o644)).To(Succeed())

	By("creating shared e2e namespace " + e2eNamespace)
	cl, err := harness.NewClient()
	Expect(err).NotTo(HaveOccurred(), "k8s client")
	shared.k8s = cl
	// On a kept VM the previous run may still be finalizing this namespace;
	// give deletion time to finish instead of failing on the first check.
	Eventually(harness.EnsureNamespace).WithArguments(ctx, cl, e2eNamespace).WithTimeout(60 * time.Second).Should(Succeed())

	By("spawning oidc-mock")
	shared.oidcProc, err = harness.Spawn(
		"/usr/local/bin/oidc-mock", nil, nil,
		"/var/log/hl-ingress/oidc.log",
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(harness.WaitForOidc(ctx, oidcBase, 60*time.Second)).To(Succeed())

	By("minting controller JWT and writing to " + tokenPath)
	jwt, err := harness.MintToken(ctx, oidcBase, controllerSub, audience, 24*time.Hour)
	Expect(err).NotTo(HaveOccurred())
	Expect(harness.WriteToken(tokenPath, jwt)).To(Succeed())

	By("writing ingress config to " + ingressConfigPath)
	Expect(harness.WriteIngressConfig(ingressConfigPath, harness.DefaultIngressConfig())).To(Succeed())

	By("spawning ingress and waiting for it to become healthy")
	shared.ingressProc, err = harness.Spawn(
		"/usr/local/bin/ingress",
		[]string{"--config", ingressConfigPath, "--state-dir", "/var/lib/homelab-ingress"},
		nil,
		"/var/log/hl-ingress/ingress.log",
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(harness.WaitForIngress(ctx, ingressAddr, caPEMPath, tokenPath, 60*time.Second)).To(Succeed())

	By("spawning controller")
	shared.controllerProc, err = harness.Spawn(
		"/usr/local/bin/controller",
		[]string{
			"--router-addr", ingressAddr,
			"--token-path", tokenPath,
			"--ca-path", caPEMPath,
			// Metrics must not collide with the controller's hardcoded
			// health-probe port (:8081) nor with oidc-mock (:8080). A
			// collision kills the manager right after it logs "starting
			// manager", so keep these three distinct.
			"--metrics-addr", ":18080",
			"--poll-interval", "5s",
		},
		[]string{"KUBECONFIG=/etc/rancher/k3s/k3s.yaml"},
		"/var/log/hl-ingress/controller.log",
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(harness.WaitForControllerReady(ctx, shared.controllerProc, controllerLog, 60*time.Second)).To(Succeed())

	// Checked here rather than in the zero-LB spec: specs run in randomized
	// order and later specs legitimately produce controller ERROR lines, so a
	// whole-file check there would be order-dependent. At this point only
	// startup has happened, which is exactly the clean baseline.
	By("asserting the controller started without ERROR log lines")
	errCount, err := harness.GrepCountFile(controllerLog, "ERROR")
	Expect(err).NotTo(HaveOccurred())
	Expect(errCount).To(BeZero(), "controller logged ERROR lines during startup; see "+controllerLog)

	By("building router gRPC client shared across scenarios")
	rClient, conn, err := harness.NewRouterClient(ingressAddr, tokenPath, caPEMPath)
	Expect(err).NotTo(HaveOccurred())
	shared.router = rClient
	shared.routerConn = conn
})

var _ = AfterSuite(func() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Delete the namespace while the controller is still running: the
	// controller owns the Service finalizer (homelab-ingress.vranyes.com/
	// cleanup). If it were stopped first, any Service still carrying the
	// finalizer could never be finalized and the namespace would stay
	// Terminating forever — which also breaks the next run on a kept VM.
	if shared.k8s != nil {
		By("deleting shared namespace " + e2eNamespace + " and waiting for finalizers")
		if err := harness.DeleteNamespaceIfExists(ctx, shared.k8s, e2eNamespace); err != nil {
			GinkgoWriter.Printf("warning: delete namespace: %v\n", err)
		}
		if err := harness.WaitForNamespaceGone(ctx, shared.k8s, e2eNamespace, 2*time.Minute); err != nil {
			GinkgoWriter.Printf("warning: namespace cleanup incomplete: %v\n", err)
		}
	}

	By("stopping controller, ingress, oidc in reverse order (best effort)")
	if shared.controllerProc != nil {
		_ = shared.controllerProc.Terminate(5 * time.Second)
	}
	if shared.ingressProc != nil {
		_ = shared.ingressProc.Terminate(5 * time.Second)
	}
	if shared.oidcProc != nil {
		_ = shared.oidcProc.Terminate(5 * time.Second)
	}
	if shared.routerConn != nil {
		_ = shared.routerConn.Close()
	}
})

// failFast shells out to Ginkgo's Fail() so the suite can be invoked from
// non-spec contexts (helpers) without going through Expect.
func failFast(msg string) {
	Fail(msg)
}

// ensureSuiteRunning verifies the shared fixtures were set up. Used by
// scenario helpers that defensively guard against nil state.
func ensureSuiteRunning() {
	if shared.k8s == nil || shared.router == nil {
		failFast("e2e fixtures not initialized; ensure BeforeSuite ran")
	}
}
