# DHCP Module

Requests and manages IPv4 addresses from a DHCP server for macvlan interfaces that
the Macvlan Manager has already provisioned.

## Architecture

Two files with a clear separation of concerns:

- **`client_interface.go`** — Orchestrator (`Manager`).
  Subscribes to state notifications, creates/stops per-interface DHCP clients, and
  runs a periodic reconciliation loop.
  Writes acquired IPs back into `State.SourceAddress` via the `onIPChange` callback,
  which triggers a new event cycle so the Macvlan and Router managers react.

- **`real.go`** — Concrete DHCP client (`realClient`) using the `github.com/insomniacslk/dhcp`
  library.
  Implements the `Client` interface (`Start`, `Stop`, `CurrentIP`, `IsAcquired`).
  Performs DHCP discover/request, handles renewals in a loop, and sends a DHCP release
  on shutdown.

Both files carry `//go:build linux` — the DHCP module only compiles on Linux.

## Manager (`client_interface.go`)

### Internal state

- `clients map[string]Client` — interface name → DHCP client
- `ifaceToService map[string]string` — reverse mapping: interface name → service
  key (used by `onIPChange` to look up which service to update when an IP arrives)

### Event handling (`Manager.Run`)

Subscribes to `LBNotification` events from the shared `State.UpdateCh`:

| Event                         | Handler        | Behavior                                                                                                                                                                                                                                                   |
|-------------------------------|----------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `CreateEvent` / `UpdateEvent` | `handleUpsert` | Creates a new `realClient` if none exists for that interface (requires non-empty interface name and non-nil MAC). If the interface name changed for a service, removes the old client first. `ensureAndStart` is idempotent — safe to call multiple times. |
| `DeleteEvent`                 | `handleDelete` | Stops the DHCP client and removes it. If the notification carries no interface name, falls back to the `ifaceToService` reverse mapping.                                                                                                                   |

### Reconciliation loop

Runs every **30 seconds** via `Manager.reconcile()`:

- Snapshots the desired services from `State.Clone()`
- Creates any DHCP clients for services that have a valid interface name and MAC
  but no active client
- Removes any DHCP clients whose interface no longer exists in state

This corrects drift after restarts or transient failures.

### IP change feedback (`onIPChange`)

When a `realClient` acquires or changes its IP, the callback:

1. Looks up the service key via `serviceKeyForInterface`
2. Clones the service, sets `SourceAddress` to the new IP and `SourceMask` to `/32`
3. Sets `DHCPAcquired = true`
4. Calls `st.UpdateService(clone)` — this emits a new `UpdateEvent`

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
            ├─ wait T1 = leaseTime/2 (min 30s, default 30min)
            ├─ dhcpRenew()   ← 30s timeout, 1 retry
            │    ├─ success → continue renewal loop
            │    └─ failure  → break to full discover/request cycle
            └─ on IP change → callback notifies Manager

Stop()
  └─ cancel context → wait for goroutine to finish → send DHCP release (10s timeout)
```

Key details:

- **Initial request**: `newDHCPConn` creates `nclient4.Client` with configurable
  timeout and retries.
  The request uses the interface's MAC address from state, so the DHCP server sees
  the same MAC as the macvlan interface.
- **Renewal interval**: Computed as T1 = `IPAddressLeaseTime / 2` from the DHCP
  ACK.
  Falls back to 60 minutes if the server doesn't advertise a lease time.
  Never goes below 30 seconds.
- **Lease release**: `Stop()` blocks until the goroutine exits, then sends a DHCP
  RELEASE message.
  If the release fails, the error is logged but does not block shutdown.
- **Edge case — no ACK**: Both `dhcpRequest` and `dhcpRenew` verify `resp.ACK` is
  non-nil before using the assigned IP.

## Shutdown sequence (from `main.go`)

```go
defer dhcpMgr.Stop()      // closes the done channel → Run() exits
defer dhcpMgr.StopAll()   // stops every client (cancels context, releases leases)
```

`Stop()` signals the event loop to exit.
`StopAll()` iterates all active clients and cancels their contexts, then sends DHCP
releases — this is critical for a clean shutdown where the DHCP server can reclaim
the addresses immediately.

## Security & constraints

- Requires **root** privileges (raw socket access for DHCP packets)
- Bound to the macvlan interface's MAC address — the DHCP server must be on the
  same L2 network
- No test coverage currently exists for the DHCP module
