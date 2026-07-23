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

// sharedContext is populated in BeforeSuite and torn down in AfterSuite.
// Scenarios read it via shared() rather than capturing the values in
// closure variables — that makes test ordering independent and avoids
// stale captures when the same scenarios re-run on developer shells.
var shared = struct {
	k8s        client.Client
	router     routerv1.RouterServiceClient
	routerConn *grpc.ClientConn
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

	By("creating shared e2e namespace " + e2eNamespace)
	cl, err := harness.NewClient()
	Expect(err).NotTo(HaveOccurred(), "k8s client")
	shared.k8s = cl
	Expect(harness.EnsureNamespace(ctx, cl, e2eNamespace)).To(Succeed())

	By("starting oidc-mock openrc service")
	Expect(harness.StartService(ctx, "hl-ingress-oidc")).To(Succeed())
	Expect(harness.WaitForOidc(ctx, oidcBase, 60*time.Second)).To(Succeed())

	By("minting controller JWT and writing to " + tokenPath)
	jwt, err := harness.MintToken(ctx, oidcBase, controllerSub, audience, 24*time.Hour)
	Expect(err).NotTo(HaveOccurred())
	Expect(harness.WriteToken(tokenPath, jwt)).To(Succeed())

	By("writing ingress config to " + ingressConfigPath)
	Expect(harness.WriteIngressConfig(ingressConfigPath, harness.DefaultIngressConfig())).To(Succeed())

	By("starting ingress openrc service and waiting for it to become healthy")
	Expect(harness.StartService(ctx, "hl-ingress-ingress")).To(Succeed())
	Expect(harness.WaitForIngress(ctx, ingressAddr, caPEMPath, tokenPath, 60*time.Second)).To(Succeed())

	By("starting controller openrc service")
	Expect(harness.StartService(ctx, "hl-ingress-controller")).To(Succeed())
	Expect(harness.WaitForControllerReady(ctx, controllerLog, 60*time.Second)).To(Succeed())

	By("building router gRPC client shared across scenarios")
	rClient, conn, err := harness.NewRouterClient(ingressAddr, tokenPath, caPEMPath)
	Expect(err).NotTo(HaveOccurred())
	shared.router = rClient
	shared.routerConn = conn
})

var _ = AfterSuite(func() {
	ctx := context.Background()
	By("stopping controller, ingress, oidc in reverse order (best effort)")
	_ = harness.StopService(ctx, "hl-ingress-controller")
	_ = harness.StopService(ctx, "hl-ingress-ingress")
	_ = harness.StopService(ctx, "hl-ingress-oidc")
	if shared.routerConn != nil {
		_ = shared.routerConn.Close()
	}
	if shared.k8s != nil {
		By("deleting shared namespace " + e2eNamespace + " (best effort)")
		_ = harness.DeleteNamespaceIfExists(ctx, shared.k8s, e2eNamespace)
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
