# HomeLab LoadBalancer Controller — Design Proposal

## Overview

Two-service system providing Kubernetes `LoadBalancer`-type Service support in a residential home-lab environment where the ISP assigns dynamic public IPs per MAC address via DHCP.

A Linux edge router runs a gRPC service that manages macvlan interfaces (each with a unique MAC), runs an embedded DHCP client per interface to obtain public IPs, and configures iptables DNAT + FORWARD rules to direct traffic to Kubernetes ClusterIPs. A Kubernetes controller running inside the cluster watches `LoadBalancer` Services and reconciles them against the router service, polling periodically for IP changes caused by DHCP lease renewals.

### Why This Architecture

- ISPs commonly assign one public IP per MAC via DHCP
- macvlan interfaces in `vepa` mode each get a unique MAC, enabling multiple public IPs on a single physical link
- K8s nodes are on the same L2 as the router, so DNAT to ClusterIP works without SNAT
- 1:1 mapping of macvlan-to-Service avoids port conflicts

### Traffic Flow

```
Internet → 81.x.x.5:443 (macvlan public IP)
         → DNAT → 10.43.108.103:443 (K8s ClusterIP)
         → FORWARD → K8s node (same L2)
         → kube-proxy → pod
         → return traffic direct (no SNAT)
```

### Architecture Diagram

```
                    ┌──────────────────────────────────────────────────┐
                    │              Linux Edge Router                    │
                    │                                                  │
      ISP ── wan.201┤  ┌────────────────────────────────────────────┐  │
                    │  │           Router Service (gRPC)             │  │
                    │  │                                              │  │
                    │  │  ┌────────────────────────────────────────┐  │  │
                    │  │  │  macvlan: lb-default-myapp             │  │  │
                    │  │  │  mode: vepa                             │  │  │
                    │  │  │  MAC: 00:58:28:28:C3:F2                │  │  │
                    │  │  │  IP: 81.x.x.5 (via DHCP from ISP)      │  │  │
                    │  │  │  DHCP client goroutine                  │  │  │
                    │  │  └────────────────────────────────────────┘  │  │
                    │  │                                              │  │
                    │  │  ┌────────────────────────────────────────┐  │  │
                    │  │  │  macvlan: lb-default-webapp            │  │  │
                    │  │  │  mode: vepa                             │  │  │
                    │  │  │  MAC: 00:58:28:A1:B4:C2                │  │  │
                    │  │  │  IP: 82.x.x.12 (via DHCP from ISP)     │  │  │
                    │  │  │  DHCP client goroutine                  │  │  │
                    │  │  └────────────────────────────────────────┘  │  │
                    │  │                                              │  │
                    │  │  ┌────────────────────────────────────────┐  │  │
                    │  │  │  Firewall:                               │  │  │
                    │  │  │  - DNAT: macvlan:port → ClusterIP:port   │  │  │
                    │  │  │  - FORWARD: allow macvlan → ClusterIP    │  │  │
                    │  │  │  - Tagged with service key for cleanup   │  │  │
                    │  │  └────────────────────────────────────────┘  │  │
                    │  └──────────────────────────┬─────────────────┘  │
                    └─────────────────────────────┼────────────────────┘
                                                  │ gRPC
                                                  │
                                    ┌─────────────┴─────────────┐
                                    │     K8s Cluster (L2)       │
                                    │                            │
                                    │  ┌──────────────────────┐  │
                                    │  │  LB Controller       │  │
                                    │  │  (Deployment)        │──┤
                                    │  └──────────┬───────────┘  │
                                    │             │              │
                                    │  ┌──────────┴───────────┐  │
                                    │  │  kube-apiserver      │  │
                                    │  │  (Service informer)   │  │
                                    │  └──────────────────────┘  │
                                    │                            │
                                    │  K8s Nodes (same L2)       │
                                    │  ClusterIPs routable       │
                                    └────────────────────────────┘
```

---

## Part 1: Router Service

Runs as a systemd-managed binary on the Linux edge router. Exposes a gRPC API and manages macvlan interfaces, DHCP, and firewall rules.

### Responsibilities

- **macvlan lifecycle**: Create and remove macvlan interfaces in `vepa` mode on the WAN device, each with a unique MAC address
- **DHCP client**: Run an embedded DHCP client per macvlan to obtain public IPs from the ISP
- **IP monitoring**: Watch for DHCP lease renewals and track IP changes
- **DNAT rules**: Manage iptables `PREROUTING` rules from macvlan IP:port to ClusterIP:port
- **FORWARD rules**: Allow traffic from macvlan interfaces to K8s ClusterIPs on declared ports only
- **State persistence**: JSON state file on disk so state survives restarts; drift repair on startup
- **Idempotency**: All operations are safe to call repeatedly

### gRPC API

```protobuf
syntax = "proto3";
package router.v1;
option go_package = "github.com/vranyes/homelab-ingress/pkg/api/router/v1";

service RouterService {
  // Allocate a macvlan, start DHCP, set up DNAT/FORWARD rules
  rpc CreateLoadBalancer(CreateLoadBalancerRequest) returns (CreateLoadBalancerResponse);

  // Update DNAT/FORWARD rules for an existing service (ports or ClusterIP changed)
  rpc UpdateLoadBalancer(UpdateLoadBalancerRequest) returns (UpdateLoadBalancerResponse);

  // Remove macvlan, stop DHCP, clean up all rules
  rpc DeleteLoadBalancer(DeleteLoadBalancerRequest) returns (DeleteLoadBalancerResponse);

  // Get current state including the latest DHCP-assigned IP
  rpc GetLoadBalancer(GetLoadBalancerRequest) returns (GetLoadBalancerResponse);

  // List all managed LoadBalancers (used by controller for periodic polling)
  rpc ListLoadBalancers(ListLoadBalancersRequest) returns (ListLoadBalancersResponse);

  // Health and status
  rpc Health(HealthRequest) returns (HealthResponse);
}

message PortMapping {
  string protocol = 1;       // "tcp" or "udp"
  int32 external_port = 2;   // Port on the macvlan public IP
  int32 cluster_ip_port = 3; // Port on the K8s ClusterIP
}

message CreateLoadBalancerRequest {
  string service_key = 1;    // Unique identifier: "namespace/name"
  string cluster_ip = 2;     // K8s ClusterIP (e.g., 10.43.108.103)
  repeated PortMapping ports = 3;
}

message CreateLoadBalancerResponse {
  string assigned_ip = 1;    // Current public IP from DHCP (empty if not yet acquired)
}

message UpdateLoadBalancerRequest {
  string service_key = 1;
  string cluster_ip = 2;     // May change if Service gets a new ClusterIP
  repeated PortMapping ports = 3;
}

message UpdateLoadBalancerResponse {}

message DeleteLoadBalancerRequest {
  string service_key = 1;
}

message DeleteLoadBalancerResponse {}

message GetLoadBalancerRequest {
  string service_key = 1;
}

message GetLoadBalancerResponse {
  string service_key = 1;
  string assigned_ip = 2;    // Current public IP (may have changed via DHCP)
  string cluster_ip = 3;
  repeated PortMapping ports = 4;
  bool dhcp_acquired = 5;    // True if DHCP has assigned an IP
  string interface_name = 6; // macvlan interface name
}

message ListLoadBalancersRequest {}

message ListLoadBalancersResponse {
  repeated GetLoadBalancerResponse load_balancers = 1;
}

message HealthRequest {}

message HealthResponse {
  bool healthy = 1;
  string base_interface = 2;
  int32 active_macvlans = 3;
}
```

### macvlan Management

- **Mode**: `vepa` (as required by the ISP setup)
- **Base device**: Configured via `base_interface` in config (e.g., `wan.201`)
- **Naming**: `lb-{namespace}-{name}` with truncation if needed (e.g., `lb-default-myapp`)
- **MAC address**: Generated with the local bit set (`x2` in the second hex digit of the first octet), persisted in the state file so the same MAC is used across restarts (ensures DHCP returns the same IP)
- **Creation**:
  ```bash
  ip link add lb-default-myapp link wan.201 type macvlan mode vepa
  ip link set lb-default-myapp address 00:58:28:28:C3:F2
  ip link set lb-default-myapp up
  ```
- **Deletion**:
  ```bash
  ip link delete lb-default-myapp
  ```

### DHCP Client

- **Library**: `github.com/insomniacslk/dhcp` — embedded pure-Go DHCP client (RFC 2131)
- **Per-interface**: Each macvlan runs its own DHCP client goroutine
- **Lifecycle**:
  1. On `CreateLoadBalancer`: create macvlan, start DHCP Discover/Request
  2. On lease acquired: update state file with assigned IP
  3. On lease renewal: update state file if IP changed
  4. On `DeleteLoadBalancer`: release DHCP lease (DHCPRELEASE), remove macvlan
  5. On service restart: restore state, re-bind to existing macvlan, monitor existing leases
- **Timeout**: DHCP request times out after 30s; `dhcp_acquired` remains false until successful

### Firewall Rules

Based on the OpenWRT DNAT redirect pattern, translated to raw iptables. Rules are tagged with the service key in a comment for clean deletion.

**Per port mapping**:

```bash
# DNAT: traffic arriving on macvlan interface → ClusterIP
iptables -t nat -A PREROUTING -i lb-default-myapp -p tcp --dport 443 \
  -j DNAT --to-destination 10.43.108.103:443 \
  -m comment --comment "homelab-lb:default/myapp"

# FORWARD: allow traffic from macvlan to ClusterIP
iptables -A FORWARD -i lb-default-myapp -d 10.43.108.103 -p tcp --dport 443 \
  -m comment --comment "homelab-lb:default/myapp" -j ACCEPT
```

**On delete**: All rules with the matching comment are removed from both `nat/PREROUTING` and `FORWARD` chains.

**On startup**: State is loaded from disk, and the service verifies that existing rules and macvlan interfaces match the persisted state. Any drift is repaired.

### IP Allocation & MAC Generation

Since there is no static IP pool (IPs come from ISP DHCP), the router manages MAC addresses instead:

- MACs are generated randomly with the local bit set
- The generated MAC is persisted in the state file
- On restart, the same MAC is reused so the ISP DHCP server returns the same IP
- MAC collision avoidance via UUID-based generation

### State File

```json
{
  "version": 1,
  "base_interface": "wan.201",
  "services": {
    "default/myapp": {
      "macaddr": "00:58:28:28:C3:F2",
      "interface": "lb-default-myapp",
      "cluster_ip": "10.43.108.103",
      "ports": [
        { "protocol": "tcp", "external_port": 443, "cluster_ip_port": 443 },
        { "protocol": "tcp", "external_port": 80, "cluster_ip_port": 80 }
      ],
      "current_ip": "81.23.45.67",
      "dhcp_acquired": true
    }
  }
}
```

Location: `/var/lib/homelab-lb/allocations.json`

### Configuration

```yaml
# /etc/homelab-lb/config.yaml
base_interface: wan.201
state_file: /var/lib/homelab-lb/allocations.json
grpc:
  listen: "0.0.0.0:50051"
```

### Directory Structure

```
router/
├── go.mod
├── cmd/
│   └── router/
│       └── main.go
├── internal/
│   ├── config/
│   │   └── config.go
│   ├── macvlan/
│   │   ├── manager.go
│   │   └── manager_test.go
│   ├── dhcp/
│   │   ├── client.go
│   │   └── client_test.go
│   ├── firewall/
│   │   └── iptables.go
│   ├── state/
│   │   ├── store.go
│   │   ├── model.go
│   │   └── store_test.go
│   └── service/
│       └── router.go
├── pkg/
│   └── api/
│       └── router/v1/
│           ├── router.pb.go
│           └── router_grpc.pb.go
└── Dockerfile
```

---

## Part 2: Kubernetes LoadBalancer Controller

Runs as a Deployment inside the cluster. Uses **controller-runtime** (`sigs.k8s.io/controller-runtime`) for Service reconciliation with informer-backed caching, leader election, automatic work queues, and retry with exponential backoff.

### Responsibilities

- Watch for Services with `spec.type: LoadBalancer`
- Create/Update/Delete LoadBalancers on the Router Service via gRPC
- Write the assigned public IP to `Service.Status.LoadBalancer.Ingress`
- Poll the Router Service periodically (default 30s) to detect DHCP-assigned IP changes
- Manage finalizers for clean deletion

### Reconcile Logic

```
Reconcile(Service key):
  1. Get Service from cache
  2. If not found or type != LoadBalancer:
     → Call RouterService.DeleteLoadBalancer (best effort)
     → Return (finalizer not present, nothing to clean)
  3. If deletion timestamp is set:
     → Call RouterService.DeleteLoadBalancer
     → Remove finalizer
     → Return
  4. Ensure finalizer "homelab-ingress.vranyes.com/cleanup" is present
  5. Get ClusterIP from Service.Spec.ClusterIP
  6. If ClusterIP is empty or "None", skip (headless service)
  7. Build PortMapping list from Service.Spec.Ports:
     → external_port = Service.Spec.Ports[i].Port
     → cluster_ip_port = Service.Spec.Ports[i].Port
     → protocol = Service.Spec.Ports[i].Protocol
  8. Check annotation "homelab-ingress.vranyes.com/skip" → return if set
  9. Get current LoadBalancer state from RouterService.GetLoadBalancer
  10. If not found or ports/cluster_ip changed:
      → Call RouterService.CreateLoadBalancer or UpdateLoadBalancer
  11. If assigned_ip differs from Service.Status.LoadBalancer.Ingress:
      → Patch Service.Status with new IP
  12. Return
```

### IP Change Polling

DHCP-assigned IPs can change when leases renew. The controller polls the Router Service periodically to detect changes:

- **Ticker**: Runs every 30 seconds (configurable via `--poll-interval`)
- **Mechanism**: Calls `ListLoadBalancers` to get all current IPs, then compares against `Service.Status.LoadBalancer.Ingress` for each managed Service
- **On change**: Patches the Service status with the new IP
- **On startup**: Full reconcile of all existing LoadBalancer Services also checks IP state

### Annotations

| Annotation | Purpose | Default |
|---|---|---|
| `homelab-ingress.vranyes.com/skip` | Skip reconciliation for this Service | `false` |

### Finalizers

```go
const LoadBalancerFinalizer = "homelab-ingress.vranyes.com/cleanup"
```

On Service deletion, the controller calls `DeleteLoadBalancer` on the Router Service, then removes the finalizer so the Service can be fully removed from etcd.

### RBAC

```yaml
rules:
  - apiGroups: [""]
    resources: ["services", "services/status"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: [""]
    resources: ["services/finalizers"]
    verbs: ["update"]
```

### Directory Structure

```
controller/
├── go.mod
├── cmd/
│   └── controller/
│       └── main.go
├── internal/
│   ├── controller/
│   │   ├── service_controller.go
│   │   ├── service_controller_test.go
│   │   └── annotations.go
│   └── config/
│       └── config.go
├── pkg/
│   └── api/
│       └── router/v1/
│           ├── router.pb.go
│           └── router_grpc.pb.go
├── deploy/
│   ├── kustomization.yaml
│   ├── namespace.yaml
│   ├── serviceaccount.yaml
│   ├── rbac/
│   │   ├── role.yaml
│   │   └── rolebinding.yaml
│   ├── configmap.yaml
│   └── deployment.yaml
└── Dockerfile
```

---

## Part 3: Shared Concerns

### Communication

- gRPC between controller and Router Service
- Controller configured with Router Service address via flag or ConfigMap
- TLS optional for MVP (internal network); mTLS as future enhancement

### Error Handling & Retries

- **Controller side**: controller-runtime automatically requeues failed reconciles with exponential backoff
- **Router Service side**: Idempotent operations; gRPC status codes for structured errors
- **DHCP failures**: `CreateLoadBalancer` returns immediately even if DHCP has not yet acquired an IP; controller polls until `dhcp_acquired` becomes true

### Observability

**Router Service**:
- Structured logging (`log/slog`)
- Prometheus metrics:
  - `homelab_lb_macvlan_count`
  - `homelab_lb_firewall_rules_total`
  - `homelab_lb_dhcp_acquired_count`
  - `homelab_lb_grpc_requests_total`
  - `homelab_lb_grpc_request_duration_seconds`

**K8s Controller**:
- Standard controller-runtime metrics (workqueue depth, reconcile errors)
- Custom metrics:
  - `homelab_lb_services_managed`
  - `homelab_lb_ip_changes_total`

### Testing Strategy

**Router Service**:
- Unit tests for IPAM/MAC generation, state management, config parsing
- Integration tests for macvlan/firewall (require root, run in CI with `NET_ADMIN` capability)
- gRPC API tests

**K8s Controller**:
- Unit tests with `envtest` (controller-runtime embedded apiserver)
- Integration tests with kind cluster

### Failure Modes

| Failure | Behavior | Recovery |
|---|---|---|
| Router Service down | Controller reconciles fail, requeued | Auto retry with backoff |
| Router Service restart | State restored; macvlan/DHCP/firewall drift repaired | Controller re-reconciles on next poll |
| DHCP fails to acquire IP | `dhcp_acquired` stays false; controller keeps polling | Retry; ISP-side issue |
| IP changes via DHCP renewal | Detected on next poll; Service.Status patched | Automatic |
| K8s API down | Controller can't update Service status | Auto retry |
| MAC collision | Retry with new MAC on Create | Handled by generation logic |

---

## Part 4: Full Project Structure

```
homelab-ingress/
├── Makefile
├── go.work                          # Go workspace for both modules
├── buf.yaml                         # Protobuf config
├── buf.gen.yaml                     # Protobuf generation config
│
├── proto/
│   └── router/v1/
│       └── router.proto             # gRPC service definition
│
├── router/                          # Router Service (runs on edge router)
│   ├── go.mod
│   ├── cmd/
│   │   └── router/
│   │       └── main.go
│   ├── internal/
│   │   ├── config/config.go
│   │   ├── macvlan/manager.go
│   │   ├── dhcp/client.go
│   │   ├── firewall/iptables.go
│   │   ├── state/store.go
│   │   ├── state/model.go
│   │   └── service/router.go
│   ├── pkg/api/router/v1/
│   └── Dockerfile
│
├── controller/                      # K8s LoadBalancer Controller
│   ├── go.mod
│   ├── cmd/
│   │   └── controller/
│   │       └── main.go
│   ├── internal/
│   │   ├── controller/
│   │   │   ├── service_controller.go
│   │   │   └── annotations.go
│   │   └── config/config.go
│   ├── pkg/api/router/v1/
│   ├── deploy/
│   │   ├── kustomization.yaml
│   │   ├── namespace.yaml
│   │   ├── serviceaccount.yaml
│   │   ├── rbac/
│   │   │   ├── role.yaml
│   │   │   └── rolebinding.yaml
│   │   ├── configmap.yaml
│   │   └── deployment.yaml
│   └── Dockerfile
│
└── docs/
    └── proposal.md                  # This file
```

---

## Part 5: Implementation Phases

### Phase 1 — Foundation

- Set up Go workspace, modules, and build tooling
- Define and generate protobuf code with `buf`
- Router Service: config loading, MAC generation, state persistence
- Router Service: macvlan manager via netlink (create/delete/ensure)
- Router Service: embedded DHCP client per macvlan

### Phase 2 — Router Service Complete

- Router Service: iptables DNAT + FORWARD rule management
- Router Service: gRPC server with Create/Update/Delete/Get/List/Health
- Router Service: startup state recovery and drift repair
- Manual testing with `grpcurl`

### Phase 3 — Kubernetes Controller

- Controller: controller-runtime project scaffold
- Controller: Service informer and reconciler
- Controller: gRPC client for Router Service
- Controller: finalizer logic
- Controller: annotation parsing
- Controller: IP change polling (periodic ticker)
- K8s manifests: RBAC, ConfigMap, Deployment
- Test with kind cluster + simulated Router Service

### Phase 4 — Integration & Production

- End-to-end testing with real router + K8s cluster
- Prometheus metrics on both services
- Structured logging
- Comprehensive unit and integration tests
- Docker images and deployment documentation

---

## Part 6: Key Dependencies

### Router Service

| Package | Purpose |
|---|---|
| `google.golang.org/grpc` | gRPC server |
| `google.golang.org/protobuf` | Protobuf runtime |
| `github.com/vishvananda/netlink` | macvlan management via netlink |
| `github.com/insomniacslk/dhcp` | Embedded DHCP client |
| `github.com/coreos/go-iptables` | iptables rule management |
| `gopkg.in/yaml.v3` | Config file parsing |
| `github.com/prometheus/client_golang` | Metrics |

### Kubernetes Controller

| Package | Purpose |
|---|---|
| `sigs.k8s.io/controller-runtime` | Controller framework |
| `google.golang.org/grpc` | gRPC client |
| `google.golang.org/protobuf` | Protobuf runtime |
| `k8s.io/api/core/v1` | K8s Service types |

---

## Development Standards

### BDD + TDD

All code is written using Behavior-Driven Design and Test-Driven Development:
1. **Write a failing test first** — define the expected behavior before any implementation
2. **Write minimal code to pass the test** — no over-engineering
3. **Refactor** — clean up while keeping tests green
4. **Repeat** — one test case at a time

Tests should describe behavior using Given/When/Then patterns where applicable. Table-driven tests are preferred for Go.

### Code Coverage

- **Minimum coverage: 90%** across all packages
- Coverage is verified via `go test -cover ./...`
- No code is merged without meeting the coverage threshold
- Integration tests (requiring root/netlink/iptables) are tagged with build constraints and excluded from coverage thresholds

---

## Part 7: Operational Notes

### Router Service Deployment

- Runs as a systemd service on the edge router
- Requires `CAP_NET_ADMIN` and `CAP_NET_RAW` capabilities
- State directory: `/var/lib/homelab-lb/`
- Config directory: `/etc/homelab-lb/`
- Log output: journald

### Controller Deployment

- Runs as a single-replica Deployment in the cluster
- ConfigMap for Router Service gRPC address and poll interval
- Leader election enabled (supports HA if scaled)

### Recovery Flow

On Router Service startup:
1. Load state from disk
2. For each service in state:
   - Verify macvlan interface exists with correct MAC; recreate if missing
   - Re-attach DHCP client to existing interface
   - Verify firewall rules exist; recreate if missing
3. Start gRPC server

This ensures that after a router reboot, everything is consistent before accepting new requests.
