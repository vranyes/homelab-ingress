# e2e — homelab-ingress end-to-end suite

This directory holds the Ginkgo e2e suite for the homelab-ingress project plus the
oidc-mock helper binary.

## What it does

The suite exercises both halves of the homelab-ingress stack in a real multi-process
topology inside a single lima VM (Alpine `linux-virt`):

- **ingress** (`ingress/cmd/ingress`) — the data-plane daemon that creates macvlan
  interfaces, gets DHCP leases via dnsmasq, and installs nftables DNAT rules.
  Spawned as a child process by the test harness.
- **controller** (`controller/cmd/controller`) — the Kubernetes controller that
  watches `Service` objects of type `LoadBalancer` with `loadBalancerClass: homelab-ingress`.
  Spawned as a child process by the test harness.
  Talks to the k3s API server via `/etc/rancher/k3s/k3s.yaml`.
- **oidc-mock** (`e2e/cmd/oidc-mock`) — a tiny mock OIDC provider used by the ingress's
  `auth.NewTokenVerifier`.
  Serves `.well-known/openid-configuration`, JWKS, and a `/token` endpoint that
  mints RS256 JWTs signed with a baked-in RSA key.
  Spawned as a child process by the test harness on `127.0.0.1:8080`.
- **traefik/whoami** — replaced by **`identicon`** (`e2e/cmd/identicon`), a tiny
  Go HTTP echo server that returns the pod hostname in the response body. When
  `backend.mode: fake` in the ingress config YAML the ingress daemon itself starts
  an identicon process as a child; for cluster-backed backends the identicon image
  needs to be loaded into k3s (see Known Issues below).

The test harness spawns the three binaries in dependency order (oidc → ingress →
controller) during `BeforeSuite` and terminates them in reverse order during
`AfterSuite`. Each process is sent SIGTERM followed by SIGKILL after a 5-second
grace period. The k3s cluster runs in the VM and schedules whoami pods.
The controller reads EndpointSlices from k3s and drives the ingress via gRPC.
The test binary itself runs in-VM as root (via `limactl exec ... go test -tags=e2e`);
it creates Service objects in k3s, mutates the OIDC token file, fires HTTP traffic
at allocated LB IPs, and inspects nftables and log files for assertions.

## Scenario matrix

| File                             | # LBs | Targets/LB | Asserts                                                                                                          |
|----------------------------------|-------|------------|------------------------------------------------------------------------------------------------------------------|
| `zero_lb_test.go`                | 0     | -          | `Health.active_macvlans == 0`; no per-LB nftables chains; controller log clean                                   |
| `one_lb_test.go` (DescribeTable) | 1     | 0          | no macvlan provisioned (controller requeues on zero ready endpoints); no status IP; no nftables chains           |
|                                  | 1     | 1          | status IP present; backend pods Ready; 50 fresh-conn GETs all return one backend ID                              |
|                                  | 1     | 3          | status IP present; backend pods Ready; 50 fresh-conn GETs return all 3 backends within ±25% uniform              |
| `two_lb_test.go`                 | 2     | 1 + 3      | distinct LB IPs; per-LB isolation; lb1 hits 1, lb2 hits 3; both per-service nftables chains exist                |
| `two_lb_test.go`                 | 2     | 1 + 1      | same external port (80) on both LBs; no cross-talk; both per-service nftables chains exist                       |
| `churn_test.go`                  | 1→0   | 1          | macvlan gone; nftables chain gone; Service finalizer removed                                                     |
| `churn_test.go`                  | 0→1   | 1          | recreate within 10s; reconverges within 30s                                                                      |
| `oidc_reject_test.go`            | 1     | 1          | mutated token (aud=bogus) rejected; controller log shows Unauthenticated; no Service status IP set               |
| `grpc_list_test.go`              | 1     | 1          | `Get` and `List` report the same converged IP; no entry ever contains `"<nil>"`                                  |

Traffic always uses fresh TCP connections (`CollectBackendIDsFreshConn`,
`DisableKeepAlives`): keep-alive reuses the same connection — and thus the
same numgen evaluation — across requests, which under-counts distinct
backends on round-robin rules. Scenarios wait for backend pods to be Ready
via `WaitForBackendReady` before creating Services so a missing backend
image fails fast with the pod's waiting reason instead of a 60s status-IP
timeout.

## Running the suite

```bash
# Install lima once (macOS):
brew install lima

# Boot the VM, build binaries, run the Ginkgo suite:
make test-e2e

# Boot and drop into an interactive shell (for manual debugging):
make test-e2e-shell

# Boot and copy the repo in without running tests:
make test-e2e-bringup

# Tear down the VM:
make remove-e2e-vm
```

The same `make test-e2e` target runs in CI via `.github/workflows/e2e.yaml`.
Both macOS (developer) and Linux (CI) hosts use the same target and the same lima
VM definition.

## Source layout

```text
e2e/
├── README.md                                this file
├── go.mod                                    standalone module `github.com/vranyes/homelab-ingress/e2e`
├── suite_test.go                             Ginkgo entry; BeforeSuite/AfterSuite wiring
├── suite_helpers_test.go                     small helpers shared across scenarios
├── zero_lb_test.go, one_lb_test.go, ...      scenario files (all in package e2e_test)
├── cmd/
│   └── oidc-mock/main.go                     mock OIDC provider (RS256, embedded PEM)
└── harness/                                  helpers imported by scenarios
    ├── vm.go                                 shell helpers, file IO
    ├── process.go                            Spawn / ManagedProcess lifecycle
    ├── k8s.go                                controller-runtime client + Service/Deployment factories
    ├── backends.go                           DeployBackend / TearDownBackend wrappers
    ├── backends_wait.go                      WaitForBackendReady (fail fast on ImagePullBackOff)
    ├── oidc.go                               MintToken / WaitForOidc
    ├── token.go                              WriteToken / MutateToken / BadAudienceToken
    ├── ingress.go                            IngressConfig marshalling + WaitForIngress
    ├── controller.go                         WaitForControllerReady / WaitForControllerReconcile
    ├── traffic.go                            CollectBackendIDsFreshConn (round-robin) / AssertAllHit / AssertRoughlyUniform
    ├── nft.go                                (#linux) LBChains / AssertNoLBChains / AssertLBChainExists
    └── log.go                                TailFile / GrepFile / WaitForLogMatch
```

## v1 caveats

- IPv6 and UDP are out of scope for v1.
- The controller's gRPC client and the e2e harness's gRPC client are kept in separate
  modules (`controller` and `e2e` respectively).
  The controller has its own vendored copy of the proto stubs and the e2e harness
  imports those (rather than the ingress module's copies) for consistency.

## Known issues

### Backend image (fixed by pre-pull + readiness gate)

`DeployIdenticon` (`harness/k8s.go`) creates a k8s Deployment using the
`traefik/whoami:latest` public container image. The Lima VM's k3s runs with
its own embedded containerd; if that image is not cached, pods fail with
`ImagePullBackOff` and every traffic spec times out in
`WaitForServiceStatusIP`.

Two mitigations are now in place:

- **Provision-time pre-pull:** `e2e-vm/scripts/04-backend-image.sh` (wired
  into `e2e-vm/lima.yaml`) pulls the exact `backendImage` reference into
  k3s containerd with retries, failing fast at provision time instead of
  per-spec.
- **Fail-fast readiness gate:** scenarios call
  `harness.WaitForBackendReady` after `DeployBackend`, so a missing image
  surfaces immediately with the pod's container waiting reason instead of a
  60s status-IP timeout.

Longer term, replacing `traefik/whoami` with a binary-backed backend that
needs no registry (the `e2e/cmd/identicon` idea noted in earlier docs)
would remove the Docker Hub dependency entirely. The traffic parser
(`harness/traffic.go:parseHostname`) currently expects the
`traefik/whoami` `Hostname: <pod>` body format, so any replacement must
keep that contract or update the parser in lockstep.

**Smoke check while iterating:** `zero_lb` and `oidc_reject` require no
k8s backend Deployment:

```bash
# Inside the VM:
cd /root/homelab-ingress/e2e
go test -tags=e2e -v -run 'ZeroLB|OidcReject|ControllerRestart' ./...
```

### Spec timing

The suite has a 40-minute timeout (`-timeout=40m`). Per-spec contexts sum to
~35 minutes worst-case (backend readiness + status IP + traffic windows
across 11 specs), so a fully green run finishes in minutes while a cascading
failure still produces a Ginkgo report instead of a `go test` SIGKILL. All
specs should complete in under 10 minutes on a healthy provision.

## RPC consistency the suite pins

`GetLoadBalancer` and `ListLoadBalancers` must agree: a DHCP-pending LB
reports `assigned_ip == ""` on both; a converged LB reports the same IP on
both. This is pinned at two levels:

- Unit: `TestListLoadBalancers_BeforeDHCP` in
  `ingress/internal/service/service_test.go`. `List` used to return the
  literal string `"<nil>"` (`net.IP(nil).String()`) for pending LBs, which
  the controller's IP poller (`controller/internal/controller/ip_poller.go`)
  then wrote into Service status as a real IP. Fixed by nil-guarding `List`
  like `Get`.
- E2E: `grpc_list_test.go` asserts `Get` and `List` report the same
  converged IP and that no `List` entry ever contains `"<nil>"`.
