# OpenWrt packaging

The router package installs four things:

| Path                                   | Source                          | Purpose                          |
| -------------------------------------- | ------------------------------- | -------------------------------- |
| `/usr/bin/homelab-ingress`             | release binary                  | daemon                           |
| `/etc/init.d/homelab-ingress`          | `files/homelab-ingress.init`    | procd service + UCI → YAML       |
| `/etc/config/homelab-ingress`          | `files/homelab-ingress.config`  | UCI settings                     |
| `/etc/homelab-ingress/config.yaml.example` | `files/config.yaml`         | full YAML schema reference       |

At every start the init script translates UCI into
`/var/etc/homelab-ingress/config.yaml` and runs the daemon against that file:

```text
/etc/config/homelab-ingress  ──▶  /var/etc/homelab-ingress/config.yaml  ──▶  homelab-ingress
        (UCI, source of truth)          (generated, world-unreadable)         (-config [-state-dir])
```

Never edit the generated file: `/var/etc` is tmpfs and the file is rewritten on
each start. `config.yaml.example` is only a reference — procd never reads it.

## Runtime paths

| Path                                    | Contents                                   | Persists across reboot |
| --------------------------------------- | ------------------------------------------ | ---------------------- |
| `/etc/homelab-ingress/`                 | `state.db` (SQLite: `services`, `dhcp_leases`), `ca.pem`, `ca-key.pem` | yes (overlay) |
| `/etc/homelab-ingress/geo.mmdb`         | country database, only with `geoip_cache_path` set | yes (overlay)  |
| `/var/lib/homelab-ingress/geo.mmdb`     | effective country-database location on OpenWrt (init-script default) | no (`/var` is tmpfs) |
| `/var/etc/homelab-ingress/config.yaml`  | generated daemon config                    | no                     |

The split is deliberate:

- **State stays on the overlay** (`state.db` + CA must survive reboot — see below).
- **Country DB defaults to tmpfs** (5–10 MB, re-downloadable; avoids flash wear
  at the cost of one download per boot). Set `geoip_cache_path` to persist it
  instead — see below.

### CA persistence

The daemon reuses `ca.pem`/`ca-key.pem` when both exist; only the **leaf**
certificate is minted afresh on every start. The CA is the trust anchor the
controller pins via its `homelab-ingress-ca` ConfigMap, so it must not change
underneath it.

Every start logs the CA fingerprint (`msg="CA certificate ready" ...
sha256=<hex>`). If it changes across restarts, `state_dir` is not persisting
(tmpfs or dropped by sysupgrade) — add it to the backup list:

```sh
echo '/etc/homelab-ingress' >> /etc/sysupgrade.conf
```

If `state.db` exists but both CA files are missing, the daemon refuses to start
instead of silently generating a new CA. Restore the two files, or move the
whole state directory aside for a deliberate reset. After a CA change, refresh
the controller's `homelab-ingress-ca` ConfigMap and restart the controller
(see `controller/README.md`):

```sh
kubectl -n default create configmap homelab-ingress-ca \
  --from-file=ca.pem=/path/to/ca.pem --dry-run=client -o yaml | kubectl apply -f -
kubectl -n default rollout restart deployment/homelab-ingress-controller
```

## Option reference

| UCI option                | YAML key                  | Default                | Notes                                                       |
| ------------------------- | ------------------------- | ---------------------- | ----------------------------------------------------------- |
| `base_interface`          | `base_interface`          | *(required)*           | Parent interface for the macvlan interfaces.                |
| `state_dir`               | — (`-state-dir` flag)     | `/etc/homelab-ingress` | Must persist. Move it only to another persistent path.      |
| `grpc_listen`             | `grpc.listen`             | `0.0.0.0:50051`        | Omit to use the daemon default.                             |
| `ws_listen`               | `ws.listen`               | `0.0.0.0:50052`        | Omit to use the daemon default.                             |
| `auth_issuer`             | `auth.issuer`             | *(required)*           |                                                             |
| `auth_audience`           | `auth.audience`           | *(required)*           | Must match the audience the controller's projected token requests. |
| `auth_allowed_subjects`   | `auth.allowed_subjects`   | *(required)*           | UCI `list`, one `sub` per entry.                            |
| `auth_trusted_root_ca`    | `auth.trusted_root_ca`    | *(unset)*              | CA file for a private OIDC issuer.                          |
| `masquerade`              | `masquerade`              | *(none)*               | UCI `list` of `input_device,output_device`.                 |
| `geoip_provider`          | `geoip.provider`          | *(unset: disabled)*    | `dbip`, `maxmind` or `mock`.                                |
| `geoip_license_key`       | `geoip.license_key`       | *(unset)*              | Required by `maxmind`.                                      |
| `geoip_edition`           | `geoip.edition`           | `GeoLite2-Country`     | `maxmind` only.                                             |
| `geoip_download_url`      | `geoip.download_url`      | *(provider default)*   | Mirror or offline fixture.                                  |
| `geoip_refresh_interval`  | `geoip.refresh_interval`  | `24h`                  | Go duration (`90m`, `12h`).                                 |
| `geoip_stale_after`       | `geoip.stale_after`       | `840h` dbip / `168h` maxmind+mock | Go duration; DB-IP updates monthly.              |
| `geoip_cache_path`        | `geoip.cache_path`        | OpenWrt: `/var/lib/.../geo.mmdb` | Daemon default is per-provider under `state_dir`. |

Notes:

- `geoip_provider` unset means country-based geo-blocking is off, and any Service carrying
  `homelab-ingress.vranyes.com/allowed-geos` is rejected (ADR 0003).
  The `homelab-ingress.vranyes.com/allowed-cidrs` static allow-list needs no
  provider and still works.
- `geoip_provider 'mock'` is for e2e fixtures only (script warns; daemon requires a readable TSV).
- `dbip` data is CC BY 4.0 and requires attribution to
  [DB-IP](https://db-ip.com).

Example:

```sh
uci set homelab-ingress.config.geoip_provider='dbip'
uci set homelab-ingress.config.geoip_cache_path='/etc/homelab-ingress/geo.mmdb'  # survive reboots
uci commit homelab-ingress
/etc/init.d/homelab-ingress restart
```

## Testing

```sh
make test-init                 # the translation layer, no OpenWrt needed
sh -n files/homelab-ingress.init
shellcheck -s sh files/homelab-ingress.init
```

Smoke-test on a device (no procd in this test):

```sh
/etc/init.d/homelab-ingress restart && logread | grep homelab-ingress
```

## Simplifications still worth doing

1. **Masquerade as UCI sections** (`config masquerade` + `option input/output`)
   instead of `"in,out"` strings; keep the old list as a deprecated fallback.
2. **Make the websocket listener disableable** (`ws.enabled` or `listen: "off"`);
   today there is no way to turn it off.
3. **Drop the translation layer entirely** (static YAML, UCI only for
   `base_interface`) — only worth it if the schema stops changing.

## History

Past bug classes (unemitted geoip, flash writes, unquoted YAML, duplicated
defaults, space-joined lists, missing schema file) are pinned by `make
test-init`. See git history.
