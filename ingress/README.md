# homelab-ingress ingress

The `ingress` module is a Linux system service that provides declarative, event-driven
ingress routing.
It receives service definitions via a gRPC API and automatically provisions macvlan
interfaces, obtains IP addresses via DHCP, and configures nftables DNAT rules to
forward traffic to backend targets. Optional per-Service country allow-lists are
enforced at prerouting, before DNAT.

## Architecture

The ingress service uses an **event-driven declarative** model.
A central `State` object holds the desired configuration and emits notifications
whenever services are created, updated, or deleted.
Independent modules subscribe to these notifications and reconcile the actual system
state to match the desired state.

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
│  │  In-memory  │───▶│  LBNotification Channel (UpdateCh)    │  │
│  │   State     │    │  CreateEvent / UpdateEvent / Delete   │  │
│  └─────────────┘    └───┬──────────────┬──────────────┬─────┘  │
│         │               │              │              │        │
│    Persist to           │              │              │        │
│    disk (JSON)          ▼              ▼              ▼        │
└───────────────────┬────────────────────────────────────────────┘
                    │
      ┌─────────────┼──────────────┬────────────────┐
      │             │              │                │
      ▼             ▼              ▼                │
┌──────────┐  ┌──────────┐  ┌──────────┐            │
│ Macvlan  │  │  Router  │  │   DHCP   │            │
│ Manager  │  │ Manager  │  │ Manager  │            │
│          │  │          │  │          │            │
│ Creates  │  │ nftables │  │ Obtains  │            │
│ deletes  │  │  DNAT    │  │   IP     │            │
│  macvlan │  │ forward  │  │  adds    │            │
│interfaces│  │  rules   │  │  state   │────────────┘
│ sets IPs │  │          │  │  update  │  triggers
└──────────┘  └──────────┘  └──────────┘   new events
```

### Event Flow

1. **External client** (Kubernetes controller or CLI) calls the gRPC API to create/update/delete
   a service
2. **Service module** writes the change to `State`, which emits an `LBNotification`
   on `UpdateCh`
3. **All modules** receive the notification and act: - **Macvlan Manager**: creates/deletes
   interfaces, sets IP addresses - **DHCP Manager**: starts/stops DHCP clients;
   when an IP is acquired it updates State, triggering a new event cycle - **Router
   Manager**: adds/removes nftables DNAT and forward rules
4. Each module also runs a **periodic reconciliation loop** to correct drift

## Modules

### State (`internal/state/`)

The central coordination layer.
Maintains the desired state of all load balancer services and acts as an event bus.

**Key types:**

- `State` — holds the service map, base interface, and the notification channel
  (`UpdateCh`)
- `LBService` — represents a single load balancer rule with interface, MAC, IP,
  and port targets
- `LBNotification` — emitted on create, update, or delete events

**Key functions:**

- `UpdateService(st, svc)` — upserts a service and emits CreateEvent or UpdateEvent
- `DeleteService(st, key)` — removes a service and emits DeleteEvent
- `Store.Load()` / `Store.Save()` — persist state to disk as JSON (atomic write)

All modules read from and write to the same `State` instance.
Mutations to State are the **only** way to trigger cross-module coordination.

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
- `Reconcile(st)` — compares actual nftables rules against desired state, adds missing
  rules and removes extraneous ones
- `RefreshGeoSets(svcCIDRs)` — flushes and repopulates the per-service geo union sets
  after a country-database reload (no chain rebuild)
- Reconciles every 5 minutes

**nftables implementation** (`internal/router/nftables/`):

- Creates a dedicated table (`homelab-ingress`) with per-service chains
- **Ingress chain** (prerouting, NAT): matches on destination IP/port and applies
  DNAT
- **Forward chain** (forward, filter): accepts post-DNAT traffic to pod IPs/ports
- **Postrouting chain**: masquerades outbound traffic
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
which the nftables manager implements as `RefreshGeoSets`. Sets are updated in
place, so a database refresh needs neither a restart nor a chain rebuild.

Country unions are large (US+CA is tens of thousands of ranges), and a single
netlink message carrying the whole set exceeds the socket limit with
`sendmsg: message too long` (EMSGSIZE). Set population is therefore split into
2,000-element chunks, each flushed separately (`geoSetChunkSize`); chunks are
cut on range boundaries (`geoSetChunks`) so an interval start and its
`IntervalEnd` always travel in the same message. nftables interval ends are
exclusive: a single address `k` is `[k, k+1)` and a range `[first,last]` is
`[first,last+1)`. The encoding and chunking are covered by real-kernel unit
tests, including a range that straddles the chunk boundary.

With a non-empty allow-list, the per-LB chain starts with:

```text
ip daddr <extIP> ip saddr not in @geo_<sn> counter drop
```

followed by the DNAT rules and the final drop. The deny rule must precede DNAT
(ADR 0001). An unknown country leaves the deny rule in place with an empty set,
so it fails closed.

### Macvlan (`internal/macvlan/`)

Manages macvlan network interfaces on the load balancer host.
Creates, configures, and deletes interfaces but does **not** determine which IP
addresses to use — that responsibility belongs to the DHCP module and the state.

**Methods on `NetlinkManager`:**

```go
func (m *NetlinkManager) Create(iface, parent string, mac net.HardwareAddr) error
func (m *NetlinkManager) Delete(iface string) error
func (m *NetlinkManager) Exists(iface string) bool
func (m *NetlinkManager) GetMAC(iface string) (net.HardwareAddr, error)
func (m *NetlinkManager) SetUp(iface string) error
func (m *NetlinkManager) SetMAC(iface string, mac net.HardwareAddr) error
func (m *NetlinkManager) SetIP(iface string, ip net.IP, subnetMask net.IPMask) error
```

**Manager behavior:**

- `Run(svc)` — subscribes to state notifications via the `LBNotification`
  channel:
  - **CreateEvent**: generates a MAC (locally-administered unicast), creates
    the macvlan interface in **VEPA mode** on the parent interface
    (`st.BaseIface`), brings it up, then **optimistically** writes the interface
    name and MAC back to State (even if creation failed, other modules are
    triggered).
  - **UpdateEvent**: sets the IP address from `SourceAddress`/`SourceMask`
    using **replace semantics** (removes all existing IPs first), and sets the
    MAC address via `SetMAC` (no-op if already correct).
  - **DeleteEvent**: removes the interface; looks up the interface name from
    State (`GetInterfaceName()`) rather than recomputing it from the service key.
- **No periodic reconciliation** — the macvlan module is purely event-driven
  (unlike the DHCP module, which reconciles every 30s, and the Router module,
  which reconciles every 5 minutes).
- Restoring state from persistence on startup does **not** automatically
  recreate macvlan interfaces — only gRPC mutations flow through the event
  channel.

**Design details:**

- **VEPA mode** (`MACVLAN_MODE_VEPA`): all frames from the macvlan interface are
  forwarded through the physical switch.
  Requires hairpin mode on the upstream switch port for inter-macvlan communication
  on the same host.
- **Locally-administered MACs**: `GenerateMAC()` uses `crypto/rand` then sets `mac[0]
  = (mac[0] & 0xFE) | 0x02`, ensuring unicast (bit 0 = 0) and locally administered
  (bit 1 = 1), avoiding OUI conflicts.
- **Deterministic interface names**: `InterfaceName()` replaces `/` with `-`, computes
  a SHA1 hash of the full service key, and formats as `lb-<short>-<hash>` (max 15
  chars, Linux `IFNAMSIZ` limit of 16 bytes including NUL).
  The short portion is truncated to fit, stripping trailing hyphens.
- **Idempotent operations**: `Delete()` succeeds if the interface doesn't exist;
  `SetMAC()` is a no-op when the MAC already matches; `SetIP()` removes stale addresses
  before adding the new one — no-ops when the address is correct.
- **Self-triggered events**: the `CreateEvent` handler calls `st.UpdateService()`,
  which emits an `UpdateEvent` back onto the same channel.
  This event is a no-op for the macvlan module (MAC already set, no IP yet) but
  triggers the DHCP module to start.

### DHCP (`internal/dhcp/`)

Obtains IP addresses for interfaces via DHCP.
Does not create interfaces — it assumes the macvlan module has already provisioned
them.

**Interface:**

```go
type Client interface {
    Start() error
    Stop()
    CurrentIP() net.IP
    IsAcquired() bool
}
```

**Manager behavior:**

- Maintains a map of interface → DHCP client
- `Run(svc)` — subscribes to state notifications: - **CreateEvent/UpdateEvent**:
  creates a new DHCP client for the interface (using the MAC from State) - **DeleteEvent**:
  stops and removes the DHCP client
- Each client runs a renewal loop (T1 = half lease time)
- When an IP is acquired or changes, an `IPChangeFunc` callback is invoked
- The callback (configured in `main.go`) updates State with the new `SourceAddress`,
  which triggers a new notification cycle that causes the macvlan module to set
  the IP and the router module to update nftables rules

### Service (`internal/service/`)

gRPC server that exposes the `RouterService` API.
This is the external interface for configuring load balancer rules.

**RPCs:**

- `CreateLoadBalancer` — defines a new service with port mappings and optional
  `allowed_geos`
- `UpdateLoadBalancer` — modifies port mappings or the geo allow-list
- `DeleteLoadBalancer` — removes a service
- `GetLoadBalancer` / `ListLoadBalancers` — query current state, including
  `allowed_geos`
- `Health` — health check with active macvlan count

All mutations flow through `state.UpdateService()` / `state.DeleteService()`, which
emit events to all modules.

## Lifecycle Example

When a client calls `CreateLoadBalancer("my-svc", [{tcp:80 → 10.0.0.1:8080}])`:

1. **Service** creates an `LBService` entry and calls `state.UpdateService()`
2. **State** emits a `CreateEvent` notification on `UpdateCh`
3. **Macvlan Manager** receives the event:
   - Generates a MAC address
   - Creates macvlan interface `lb-my-sv-<hash>` on the parent interface
   - Brings the interface up
   - Writes interface name and MAC back to State (triggering an UpdateEvent)
4. **DHCP Manager** receives the event:
   - Creates a DHCP client for the new interface
   - Starts DHCP discovery
5. When DHCP acquires an IP:
   - The `IPChangeFunc` callback updates State with `SourceAddress`
   - State emits an `UpdateEvent`
6. **Macvlan Manager** receives the update:
   - Sets the IP address on the interface
7. **Router Manager** receives the update:
   - Creates nftables DNAT rules for `SourceAddress:80 → 10.0.0.1:8080`

## Startup Sequence

In `cmd/ingress/main.go`:

1. Load configuration and state (or create new state)
2. Start Macvlan Manager goroutine (subscribes to `UpdateCh`)
3. Start DHCP Manager goroutine (subscribes to `UpdateCh`)
4. If `geoip.provider` is set: create the `geoip` manager, start its refresh
   goroutine, start the `geo` watcher, and wire `OnRebuild` to
   `nftables.RefreshGeoSets`
5. Start gRPC server (Service module)
6. On shutdown: graceful gRPC stop, DHCP clients stopped (releasing leases)

## State Schema

```json
{
  "version": 1,
  "base_interface": "eth0",
  "services": {
    "my-service": {
      "service_name": "my-service",
      "interface": "lb-my-se-abc123",
      "macaddr": "02:ab:cd:ef:01:23",
      "dhcp_acquired": true,
      "source_address": "192.168.1.100",
      "source_mask": "ffffff00",
      "allowed_geos": ["US", "CA"],
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

## Project Layout

```bash
ingress/
├── cmd/
│   ├── ingress/    # Main binary entrypoint
│   └── cli/             # CLI tool
├── internal/
│   ├── auth/            # gRPC authentication interceptors
│   ├── config/          # Configuration loading
│   ├── dhcp/            # DHCP client management
│   ├── geo/             # database watcher + per-service CIDR rebuild
│   ├── geoip/           # MaxMind/DB-IP providers, downloader, TSV mock
│   ├── macvlan/         # Macvlan interface management
│   ├── router/          # Router manager interface + nftables implementation
│   │   └── nftables/    # nftables rule management
│   ├── service/         # gRPC service implementation
│   └── state/           # Central state store and event bus
├── pkg/api/router/v1/   # Generated protobuf/gRPC code
```

## Requirements

- Linux host with kernel networking support
- `nftables` kernel module loaded
- Root privileges (netlink, interface creation, nftables)
- Built with `//go:build linux` tag

## Quick Start

```bash
# Build
go build ./cmd/ingress

# Run (requires root for network operations)
sudo ./ingress --config /etc/homelab-ingress/config.yaml
```

## Testing

```bash
go test ./...
```

The nftables and macvlan packages use `//go:build linux` build tags.
Tests that require kernel access are separated from unit tests that use stub implementations.
