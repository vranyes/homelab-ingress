# homelab-ingress ingress

The `ingress` module is a Linux service that provides declarative, event-driven
ingress routing: clients write service definitions via gRPC to a central `State`
store, which emits create/update/delete notifications; macvlan, DHCP, and router
managers subscribe and reconcile system state (interfaces, addresses, nftables
DNAT). Optional per-Service country allow-lists are enforced at prerouting,
before DNAT.

## Architecture

```text
┌─────────────────────────────────────────────────────────────────┐
│                        gRPC API (service)                       │
│  CreateLoadBalancer / UpdateLoadBalancer / DeleteLoadBalancer   │
└──────────────────────────────┬──────────────────────────────────┘
                               │ writes to
                               ▼
┌────────────────────────────────────────────────────────────────┐
│                          State Store                           │
│  ┌─────────────┐    ┌───────────────────────────────────────┐  │
│  │  In-memory  │───▶│  subscriber channels (Subscribe())    │  │
│  │   State     │    │  CreateEvent / UpdateEvent /          │  │
│  │             │    │  DeleteEvent                          │  │
│  └─────────────┘    └───┬──────────────┬──────────────┬─────┘  │
│         │               │              │              │        │
│    Persist to           │              │              │        │
│  disk (SQLite)         ▼              ▼              ▼        │
└───────────────────┬────────────────────────────────────────────┘
                    │
      ┌─────────────┼──────────────┬────────────────┐
      │             │              │                │
      ▼             ▼              ▼                │
┌──────────┐  ┌──────────┐  ┌──────────┐            │
│ Macvlan  │  │  Router  │  │   DHCP   │            │
│ Manager  │  │ Manager  │  │ Manager  │────────────┘
└──────────┘  └──────────┘  └──────────┘   triggers
                                           new events
```

### Event Flow

An external client (controller or CLI) calls the gRPC API; the Service module
writes to `State`, which notifies all subscribers. Macvlan creates/deletes
interfaces and sets IPs, DHCP starts/stops clients (an acquired IP is written
back to `State`, triggering a new event cycle), and Router adds/removes
nftables rules. Each module also reconciles periodically to correct drift
(macvlan and DHCP every 30s; router after 5 event-free minutes).

## Modules

### State (`internal/state/`)

Central coordination layer: holds desired state for all services and acts as
the event bus. All modules share one `State` instance; all mutations flow
through `UpdateService()` / `DeleteService()`, which emit events to all
modules.

**Key types:**

- `State` — holds the service map, base interface, and the subscriber channels
  (see `Subscribe()`)
- `LBService` — represents a single load balancer rule with interface, MAC, IP,
  and port targets
- `LBNotification` — emitted on create, update, or delete events

**Key functions:**

- `(*State).UpdateService(svc)` — upserts a service and emits CreateEvent or UpdateEvent
- `(*State).DeleteService(key)` — removes a service and emits DeleteEvent
- `Store` — persists services and DHCP leases to SQLite (`state.db`)

### Router (`internal/router/`)

Manages nftables firewall rules that forward traffic from ingress IPs/ports to backend
targets.

**Interface:**

```go
type Implementor interface {
    AddRules(service state.LBService) error
    RemoveRules(serviceKey string) error
    ListRules() ([]string, error)
}
```

**Manager behavior:**

- `Run(notif)` — subscribes to state notifications; adds rules on Create/Update,
  removes on Delete
- `Reconcile()` — re-applies rules for every known service (idempotent re-add).
  Note: no GC of unknown chains — stale chains are removed only via Delete events.
- Reconciles after 5 event-free minutes (see Event Flow)
- `RefreshGeoSets(svcCIDRs)` — flushes and repopulates the per-service geo union sets
  after a country-database reload; in place, no chain rebuild, no restart

**nftables implementation** (`internal/router/nftables/`):

- Creates a dedicated table (`homelab_ingress`) with per-service `pre_<sn>` chains
  (`sn` is the `safeName` hash of the service key)
- **Ingress chain** (`pre_<sn>`, prerouting, NAT): matches on destination IP/port and applies
  DNAT
- **Forward chain** (forward, filter): accepts `ct state established,related` and
  `ct status dnat` traffic only — no per-IP/port matches
- **Postrouting chain**: masquerades outbound traffic, but only for pairs
  configured via `WithMasquerade` / `masquerade` in the config (default: none)
- Supports single-destination DNAT and round-robin load balancing via nftables sets/maps
- Per-service, per-source-IP new-connection rate limiting: a dynamic set
  (`rl_<sn>_<rate>_<burst>`) holds one token bucket per client IP, and a
  `meter`-style `limit rate over` expression drops excess packets. The set name
  includes the configuration, so changing rate/burst starts a fresh bucket; idle
  buckets expire after 10 minutes (see ADR 0007)

### Geo-blocking (`internal/geoip/` and `internal/geo/`)

`geoip.Manager` owns a country database and refreshes it in-process:

- `provider: dbip` — free DB-IP IP-to-Country Lite MMDB, no key, monthly.
- `provider: maxmind` — GeoLite2, license key required, daily refresh.
- `provider: mock` — TSV fixture for e2e.

The `geoip` block in the ingress config is optional. With no provider set,
geo-blocking is disabled and requests carrying `allowed_geos` are rejected.
DB-IP data is CC BY 4.0; the daemon logs the required attribution at startup.

`geo.Manager` watches the database file (fsnotify plus a 30s poll fallback),
rebuilds the per-service country→CIDR map from State, and invokes `OnRebuild`,
which the nftables manager implements as `RefreshGeoSets`.

Set population is chunked into 2,000-element batches (unions exceed the
netlink `EMSGSIZE` limit), cut on range boundaries. Interval ends are
exclusive (`k` → `[k,k+1)`), covered by real-kernel unit tests.

With a non-empty allow-list, the per-LB chain starts with:

```text
ip daddr <extIP> ip saddr not in @geo_<sn> counter drop
```

followed by the DNAT rules and the final drop. The deny rule must precede DNAT
(ADR 0001). An unknown country leaves the deny rule in place with an empty set,
so it fails closed.

### Macvlan (`internal/macvlan/`)

Manages macvlan network interfaces: creates, configures, deletes. It does
**not** choose addresses — those come from DHCP via `State`.

`NetlinkManager`: `Create` / `Delete` / `Exists` / `GetMAC` / `SetUp` /
`SetMAC` / `SetIP`.

| Event | Action |
|---|---|
| `CreateEvent` | Generate locally-administered unicast MAC, create VEPA interface on `st.BaseIface`, bring up, write interface + MAC back to `State` (emits `UpdateEvent`, which starts DHCP). Creation failure logs and touches no state. |
| `UpdateEvent` | Set IP from `SourceAddress`/`SourceMask` (replace semantics); `SetMAC` (no-op if correct). |
| `DeleteEvent` | Delete the interface (name recomputed via `InterfaceName()`). |

Plus 30s reconciliation: recreates missing interfaces and re-applies IP/MAC,
so restored state converges without waiting for a gRPC mutation.

Details: names are `lb-<short>-<hash>` (SHA1, max 15 chars); operations are
idempotent.

### DHCP (`internal/dhcp/`)

Obtains IP addresses for macvlan-provisioned interfaces via DHCP.

**Interface:**

```go
type Client interface {
    Start() error
    Stop()
    CurrentIP() net.IP
    IsAcquired() bool
    LeaseInfo() *LeaseInfo
}
```

**Manager behavior:**

- Maintains a map of interface → DHCP client
- `Run(svc)` — on Create/Update starts a DHCP client for the interface (MAC
  from State); on Delete stops/removes it
- Each client runs a renewal loop (T1 = half the advertised lease, minimum 30s)
- `onIPChange` writes `SourceAddress` (+ mask from ACK/restored lease) to State →
  `UpdateEvent` → macvlan sets IP, router updates rules

### Service (`internal/service/`)

External gRPC interface (`RouterService`) for configuring load balancer rules.

**RPCs:**

- `CreateLoadBalancer` — defines a new service with port mappings and optional
  `allowed_geos`
- `UpdateLoadBalancer` — modifies port mappings or the geo allow-list
- `DeleteLoadBalancer` — removes a service
- `GetLoadBalancer` / `ListLoadBalancers` — query current state, including
  `allowed_geos`
- `Health` — health check with active macvlan count (`Healthy`, `ActiveMacvlans`).
  The proto also defines `BaseInterface`, but the daemon currently leaves it
  empty.

## Lifecycle Example

`CreateLoadBalancer` → `State` update + `CreateEvent` → macvlan creates the
interface → DHCP acquires the IP → `State` update → macvlan sets the IP,
router writes DNAT rules.

## Startup Sequence

In `cmd/ingress/main.go`:

1. Load configuration and state (SQLite `state.db`), reusing or creating the CA
2. Start Macvlan, DHCP, and Router manager goroutines (each subscribes to `State`)
3. If `geoip.provider` is set: create the `geoip` manager, start its refresh
   goroutine, start the `geo` watcher, and wire `OnRebuild` to
   `nftables.RefreshGeoSets`
4. Start the always-TLS gRPC server with OIDC interceptors, plus the optional
   WebSocket event server
5. On shutdown: graceful gRPC stop, stop clients, delete interfaces, drop the
   nftables table

## State Schema

```json
{
  "version": 1,
  "base_interface": "eth0",
  "services": {
    "my-service": {
      "service_name": "my-service",
      "interface": "lb-my-se-abc123",
      "macaddr": "AgEEAQIDBAU=",
      "dhcp_acquired": true,
      "source_address": "192.168.1.100",
      "source_mask": "////AA==",
      "allowed_geos": ["US", "CA"],
      "rate_limit": { "rate": 5, "burst": 10 },
      "targets": [
        {
          "protocol": "tcp",
          "external_port": 80,
          "destinations": [
            { "port": 8080, "address": "10.0.0.1" }
          ]
        }
      ]
    }
  }
}
```

(`macaddr`/`source_mask` serialise as base64; services live in the `services`
table of `state.db`, leases in `dhcp_leases`.)

## Project Layout

```bash
ingress/
├── cmd/
│   ├── ingress/    # Main binary entrypoint
│   └── hictl/      # CLI tool
├── internal/
│   ├── auth/            # gRPC authentication interceptors
│   ├── certgen/         # CA + leaf certificate management
│   ├── config/          # Configuration loading
│   ├── dhcp/            # DHCP client management
│   ├── geo/             # database watcher + per-service CIDR rebuild
│   ├── geoip/           # MaxMind/DB-IP providers, downloader, TSV mock
│   ├── macvlan/         # Macvlan interface management
│   ├── router/          # Router manager interface + nftables implementation
│   │   └── nftables/    # nftables rule management
│   ├── service/         # gRPC service implementation
│   ├── state/           # Central state store and event bus
│   └── ws/              # WebSocket event server
├── pkg/api/router/v1/  # Generated protobuf/gRPC code (source: proto/router/v1/)
```

## Requirements

- Linux host with nftables kernel support (//go:build linux)
- Root privileges (netlink, interface creation, nftables)

## Quick Start

```bash
# Build
go build ./cmd/ingress

# Run (requires root for network operations)
sudo ./ingress --config /etc/homelab-ingress/config.yaml
```

## Testing

```bash
go test ./...   # linux-only packages need a Linux kernel (see e2e-vm/RUNBOOK.md)
```
