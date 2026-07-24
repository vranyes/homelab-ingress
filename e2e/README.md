# e2e — homelab-ingress end-to-end suite

This directory holds the Ginkgo e2e suite for the homelab-ingress project plus the
oidc-mock helper binary.

## What it does

The suite exercises both halves of the homelab-ingress stack in a real multi-process
topology inside a single lima VM (Alpine `linux-virt`):

- **ingress** (`ingress/cmd/ingress`) — the data-plane daemon that creates macvlan
  interfaces, gets DHCP leases via dnsmasq, and installs nftables DNAT rules.
  Runs in-VM under the `hl-ingress-ingress` openrc service.
- **controller** (`controller/cmd/controller`) — the Kubernetes controller that
  watches `Service` objects of type `LoadBalancer` with `loadBalancerClass: homelab-ingress`.
  Runs in-VM as a plain process under the `hl-ingress-controller` openrc service.
  Talks to the k3s API server via `/etc/rancher/k3s/k3s.yaml`.
- **oidc-mock** (`e2e/cmd/oidc-mock`) — a tiny mock OIDC provider used by the ingress's
  `auth.NewTokenVerifier`.
  Serves `.well-known/openid-configuration`, JWKS, and a `/token` endpoint that
  mints RS256 JWTs signed with a baked-in RSA key.
  Runs as a plain VM process on `127.0.0.1:8080` under the `hl-ingress-oidc` openrc
  service.
- **traefik/whoami** — replaced by **`identicon`** (`e2e/cmd/identicon`), a tiny
  Go HTTP echo server that returns the pod hostname in the response body. When
  `backend.mode: fake` in the ingress config YAML the ingress daemon itself starts
  an identicon process as a child; for cluster-backed backends the identicon image
  needs to be loaded into k3s (see Known Issues below).

The k3s cluster runs in the VM and schedules whoami pods.
The controller reads EndpointSlices from k3s and drives the ingress via gRPC.
The test binary itself runs in-VM as root (via `limactl exec ... go test -tags=e2e`);
it creates Service objects in k3s, mutates the OIDC token file, fires HTTP traffic
at allocated LB IPs, and inspects nftables and log files for assertions.

## Scenario matrix

| File                             | # LBs | Targets/LB | Asserts                                                                                            |
|----------------------------------|-------|------------|----------------------------------------------------------------------------------------------------|
| `zero_lb_test.go`                | 0     | -          | `Health.active_macvlans == 0`; no per-LB nftables chains; controller log clean                     |
| `one_lb_test.go` (DescribeTable) | 1     | 0          | macvlan created but no Service status IP; no HTTP traffic succeeds                                 |
|                                  | 1     | 1          | status IP present; 20 GETs all return one backend ID                                               |
|                                  | 1     | 3          | status IP present; 20 GETs return ≥2 distinct backends within ±25% uniform                         |
| `two_lb_test.go`                 | 2     | 1 + 3      | distinct LB IPs; per-LB isolation; lb1 hits 1, lb2 hits 3                                          |
| `two_lb_test.go`                 | 2     | 1 + 1      | same external port (80) on both LBs; no cross-talk between backends                                |
| `churn_test.go`                  | 1→0   | 1          | macvlan gone; nftables chain gone; Service finalizer removed                                       |
| `churn_test.go`                  | 0→1   | 1          | recreate within 10s; reconverges within 30s                                                        |
| `oidc_reject_test.go`            | 1     | 1          | mutated token (aud=bogus) rejected; controller log shows Unauthenticated; no Service status IP set |
| `controller_restart_test.go`     | 1     | 3          | `pkill -9 controller`; openrc respawns; status IP unchanged; round-robin resumes                   |

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
    ├── vm.go                                 openrc service control, file IO
    ├── k8s.go                                controller-runtime client + Service/Deployment factories
    ├── backends.go                           DeployBackend / TearDownBackend wrappers
    ├── oidc.go                               MintToken / WaitForOidc
    ├── token.go                              WriteToken / MutateToken / BadAudienceToken
    ├── ingress.go                            IngressConfig marshalling + WaitForIngress
    ├── controller.go                         WaitForControllerReady / WaitForControllerReconcile
    ├── traffic.go                            CollectBackendIDs / AssertAllHit / AssertRoughlyUniform
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

### Backend Deployment fails with ImagePullBackOff (blocks 7/10 specs)

`DeployIdenticon` (harness/k8s.go) creates a k8s Deployment using the
`traefik/whoami` public container image. The Lima VM's k3s runs with its own
embedded containerd that has **no internet access** — it cannot pull from
Docker Hub. Pods fail with `ImagePullBackOff`.

Affected specs:
- `one_lb_test.go` (1 target, 3 targets)
- `two_lb_test.go` (both specs)
- `churn_test.go` (re-create step: 0→1 targets)

**To fix permanently:** pre-pull the `traefik/whoami` image into k3s's
containerd, or replace it with a binary-backed backend that doesn't need a
container registry. See `e2e-vm/RUNBOOK.md` for the debug guide.

**`zero_lb`**, **`oidc_reject`**, and **`controller_restart`** pass — they
require no k8s backend Deployment. Run them to verify data-plane and
controller logic while the image issue is outstanding:

```bash
# Inside the VM:
cd /root/homelab-ingress/e2e
go test -tags=e2e -v -run 'ZeroLB|OidcReject|ControllerRestart' ./...
```

### Spec timing

The suite has a 25-minute timeout (`-timeout=25m`). All 10 specs should
complete in under 5 minutes once the image issue is resolved.

## Open issues the suite surfaces

The `one_lb_0_targets` entry exercises the part of `ingress/internal/service` where
`ListLoadBalancers` (service.go:132) might dereference a nil `SourceAddress`.
The suite pins the corresponding behaviour — if the bug exists it manifests as a
gRPC `Internal` status on List/Get; if it has been fixed, it manifests as a normal
response with `assigned_ip == ""`.
The scenarios are written to tolerate the latter and surface the former as a test
failure when the nil-deref panic is observed.
