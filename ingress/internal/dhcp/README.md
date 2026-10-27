# DHCP Module

Requests and manages IPv4 addresses from a DHCP server for macvlan interfaces that
the Macvlan Manager has already provisioned.

## Architecture

Three files:

- **`client_interface.go`** — Orchestrator (`Manager`).
  Subscribes to state notifications, creates/stops per-interface DHCP clients, and
  runs a periodic reconciliation loop.

- **`real.go`** — Concrete DHCP client (`realClient`) using the `github.com/insomniacslk/dhcp`
  library.
  Implements the `Client` interface (`Start`, `Stop`, `CurrentIP`, `IsAcquired`,
  `LeaseInfo()`).
  Performs DHCP discover/request, handles renewals in a loop, and sends a DHCP release
  on shutdown.

- **`lease.go`** — `LeaseInfo` persistence helpers (portable, no build tag).
  Restored leases let a client skip discovery after restart.

Linux-only (`//go:build linux` on `client_interface.go`, `real.go`).

## Manager (`client_interface.go`)

### Internal state

- `clients map[string]Client` — interface name → DHCP client
- `ifaceToService map[string]string` — interface name → service key

### Event handling (`Manager.Run`)

Subscribes to `LBNotification` events from `State`:

| Event                         | Handler        | Behavior                                                        |
|-------------------------------|----------------|-----------------------------------------------------------------|
| `CreateEvent` / `UpdateEvent` | `handleUpsert` | Creates client if missing (requires interface name + MAC); replaces stale client on interface rename. |
| `DeleteEvent`                 | `handleDelete` | Stops and removes client; falls back to `ifaceToService` if name missing. |

### Reconciliation loop

Runs every **30 seconds** via `Manager.reconcile()`:

- Creates missing clients with valid interface + MAC
- Removes clients with no matching service

### IP change feedback (`onIPChange`)

Looks up the service, sets `SourceAddress`/`SourceMask`/`DHCPAcquired=true`, and
calls `st.UpdateService()` to emit an `UpdateEvent` — but only when the IP or
mask actually changed (change-guard). The lease is persisted unconditionally
(`persistLease`), so a restart restores it. The mask comes from the DHCP ACK
(or the restored lease).

This is how the DHCP module feeds back into the system: the new event triggers the
Macvlan Manager to set the IP on the interface and the Router Manager to create
nftables DNAT rules.

## realClient lifecycle (`real.go`)

```go
Start()
  └─ run() goroutine
       ├─ dhcpRequest()   ← 30s timeout, 3 retries
       │    ├─ success → enter renewal loop
       │    └─ failure  → wait 5s, retry
       └─ renewal loop
            ├─ wait T1 = leaseTime/2 (min 30s)
            ├─ dhcpRenew()   ← 30s timeout, 1 retry
            │    ├─ success → continue renewal loop
            │    └─ failure  → break to full discover/request cycle
            └─ on IP change → callback notifies Manager

Stop()
  └─ cancel context → wait for goroutine to finish → send DHCP release (10s timeout)
```

Key details:

- **MAC**: the wire request uses the kernel interface address via
  `nclient4.New(iface, ...)`; the stored MAC is only used for logging.
- **Release**: `Stop()` waits for the goroutine, then sends DHCP RELEASE (failure
  is logged, never blocks shutdown).

## Shutdown sequence (from `main.go`)

```go
grpcServer.GracefulStop()
dhcpMgr.Stop()      // event loop (also deferred)
dhcpMgr.StopAll()   // all clients + RELEASE (also deferred)
// ... 2s drain for RELEASE frames, then linkMgr.DeleteAll(), ParentSetDown(), iml.Cleanup()
```

## Security & constraints

- Requires **root** privileges (raw socket access for DHCP packets)
- DHCP server must be on same L2 network
- Unit-tested via fake `Client`; raw-socket paths need Linux (`make test-unit-vm`)
