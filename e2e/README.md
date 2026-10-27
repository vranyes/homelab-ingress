# e2e — homelab-ingress end-to-end suite

This directory holds the Ginkgo e2e suite for the homelab-ingress project plus the
oidc-mock helper binary.

## What it does

The suite exercises the full homelab-ingress stack in a multi-process
topology inside a single lima VM (Alpine `linux-virt`):

- **ingress** (`ingress/cmd/ingress`) — the data-plane daemon that creates macvlan
  interfaces, gets DHCP leases via dnsmasq, and installs nftables DNAT rules.
- **controller** (`controller/cmd/controller`) — the Kubernetes controller that
  watches `Service` objects of type `LoadBalancer` with `loadBalancerClass: homelab-ingress`.
  Talks to the k3s API server via `/etc/rancher/k3s/k3s.yaml`.
- **oidc-mock** (`e2e/cmd/oidc-mock`) — a mock OIDC provider used by the ingress's
  `auth.NewTokenVerifier`, on `127.0.0.1:8080`.
  Serves `.well-known/openid-configuration`, JWKS, and a `/token` endpoint that
  mints RS256 JWTs signed with a baked-in RSA key.
- **traefik/whoami** — the cluster-backed backend: `DeployBackend`
  (`harness/backends.go` → `DeployIdenticon` in `harness/k8s.go`) creates a
  k3s Deployment.

The harness spawns oidc-mock → ingress → controller in `BeforeSuite`
(SIGTERM then SIGKILL after a 5s grace period in `AfterSuite`). The k3s cluster
runs in the VM and schedules whoami pods. The controller reads EndpointSlices
from k3s and drives the ingress via gRPC. The test binary itself runs in-VM as
root (via `limactl shell ... go test -tags=e2e`); it creates Service objects in
k3s, mutates the OIDC token file, fires HTTP traffic at allocated LB IPs, and
inspects nftables and log files for assertions.

## Scenario matrix

| File                             | # LBs | Targets/LB | Asserts                                                                                                          |
|----------------------------------|-------|------------|------------------------------------------------------------------------------------------------------------------|
| `zero_lb_test.go`                | 0     | -          | `Health.active_macvlans == 0`; no per-LB nftables chains (`ERROR`-free controller log is asserted in `BeforeSuite`) |
| `one_lb_test.go` (DescribeTable) | 1     | 0          | no macvlan provisioned (controller requeues on zero ready endpoints); no status IP; no nftables chains           |
|                                  | 1     | 1          | status IP present; backend pods Ready; 50 fresh-conn GETs all return one backend ID                              |
|                                  | 1     | 3          | status IP present; backend pods Ready; 50 fresh-conn GETs return all 3 backends within ±25% uniform              |
| `two_lb_test.go`                 | 2     | 1 + 3      | distinct LB IPs; per-LB isolation; lb1 hits 1, lb2 hits 3; both per-service nftables chains exist                |
| `two_lb_test.go`                 | 2     | 1 + 1      | same external port (80) on both LBs; no cross-talk; both per-service nftables chains exist                       |
| `churn_test.go`                  | 1→0   | 1          | macvlan gone; nftables chain gone; Service finalizer removed                                                     |
| `churn_test.go`                  | 0→1   | 1          | recreate reconverges within 30s (traffic flows)                                                                  |
| `oidc_reject_test.go`            | 1     | 0          | mutated token (aud=bogus) rejected; controller log shows Unauthenticated; no Service status IP set (no backend)  |
| `grpc_list_test.go`              | 1     | 1          | `Get` and `List` report the same converged IP; no entry ever contains `"<nil>"`                                  |
| `dhcp_lease_test.go`             | 1     | 1          | dhcp black-box: acquisition/release (renewal timers and daemon-restart persist-restore are out of scope)         |
| `two_lb_masq_test.go`            | 1     | 2          | two-lb-masq counter validation                                               |
| `geo_block_test.go`              | 1–2   | 1          | geo deny precedes DNAT; US allowed/EU dropped; annotation flip; DB hot-reload; per-service sets                  |
| `rate_limit_test.go`             | 1     | 1          | per-client new-conn limit; bucket refill; live rate change rotates the set; removal; client isolation |

### Traffic generation

All traffic originates in `ns-server` via `CollectBackendIDsFromNS` with fresh
connections (no keep-alive, else round-robin under-counts). Host-namespace
traffic would bypass the ingress chains via kube-proxy's OUTPUT chain, so
assertions built on it stay green even when ingress rules are broken.
Rate-limit specs use `AttemptParallelFromNS` with range assertions
(`1..burst+2` successes for a flood): the token bucket refills while the
processes start.

Geo-blocking specs use a TSV-backed mock geo database at
`/var/lib/homelab-ingress/mock-geo.tsv` (selected by `geoip.provider: mock`).
It maps four `/26` slices of `10.44.0.0/24` to US, CA, EU and XX. Specs that
need an EU source add `10.44.0.129/24` to `ns-server`'s `veth-outer` and bind
curl with `--interface`. `BeforeSuite` rewrites the fixture atomically and
resets the in-netns dnsmasq lease pool (91 addresses), so a reused VM starts
clean.

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
├── zero_lb_test.go, one_lb_test.go, ...      scenario files (all in package e2e_test;
│   see matrix above for per-file coverage)
├── cmd/
│   └── oidc-mock/main.go                     mock OIDC provider (RS256, embedded PEM)
└── harness/                                  helpers imported by scenarios
    ├── vm.go                                 shell helpers, file IO, atomic writes, dnsmasq lease reset
    ├── process.go (+ process_test.go)        Spawn / ManagedProcess lifecycle
    ├── k8s.go                                k8s client, Service/Deployment factories (cached backend image), CreateLBServiceWithGeos / SetAllowedGeos
    ├── backends.go (+ backends_wait.go)        DeployBackend/TearDownBackend + WaitForBackendReady gate
    ├── oidc.go                               MintToken / WaitForOidc
    ├── token.go                              WriteToken / MutateToken / BadAudienceToken
    ├── ingress.go                            IngressConfig marshalling + WaitForIngress
    ├── controller.go                         WaitForControllerReady (log + process liveness)
    ├── traffic.go (+ traffic_test.go)        CollectBackendIDsFromNS / source-bound and attempt helpers / AssertAllHit / AssertRoughlyUniform
    ├── nft.go                                (#linux) LBChains / AssertNoLBChains / AssertLBChainExists / GetGeoSet / ClassifyRules
    └── log.go                                TailFile / GrepFile / WaitForLogMatch
```

## Caveats

- IPv6 and UDP are out of scope for v1.
- Canonical proto source is `proto/router/v1/router.proto` (both
  `ingress`/`controller` `pkg/api/router/v1` stubs are generated from it;
  the e2e harness imports the controller's copies).

## Known issues

### Backend image (fixed by pre-pull + readiness gate)

`DeployBackend` (`harness/backends.go`) creates a k8s Deployment using the
`traefik/whoami:latest` public container image (defined in `harness/k8s.go`). The Lima VM's k3s runs with
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

## Iterating (smoke check)

`zero_lb` and `oidc_reject` require no k8s backend Deployment:

```bash
# Inside the VM:
cd /root/homelab-ingress/e2e
go test -tags=e2e -count=1 -v --ginkgo.focus='zero-lb|oidc rejection' ./...
```

Always pass `-count=1`: the suite's result depends on VM and cluster state
that `go test`'s cache cannot see, so a cached "pass" is meaningless.

## Spec timing

Timeout is `-timeout=40m`; a healthy run completes in under 15 minutes.

## RPC consistency the suite pins

`GetLoadBalancer` and `ListLoadBalancers` must agree: `""` when DHCP-pending,
same IP when converged, never `"<nil>"`. Pinned by
`TestListLoadBalancers_BeforeDHCP` (unit) and `grpc_list_test.go` (e2e).
