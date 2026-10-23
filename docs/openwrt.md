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
        (UCI, source of truth)          (generated, world-unreadable)         (-config -state-dir)
```

Never edit the generated file: `/var/etc` is tmpfs and the file is rewritten on
each start. `config.yaml.example` is only a reference — procd never reads it.

## Runtime paths

| Path                                    | Contents                                   | Persists across reboot |
| --------------------------------------- | ------------------------------------------ | ---------------------- |
| `/etc/homelab-ingress/`                 | `state.db`, `ca.pem`, `ca-key.pem`         | yes (overlay)          |
| `/etc/homelab-ingress/geo.mmdb`         | country database, only with `geoip_cache_path` set | yes (overlay)  |
| `/var/lib/homelab-ingress/geo.mmdb`     | default country-database location          | no (`/var` is tmpfs)   |
| `/var/etc/homelab-ingress/config.yaml`  | generated daemon config                    | no                     |

The split is deliberate:

- **State stays on the overlay.** `ca.pem` is pinned by the controller via a
  ConfigMap, so a CA regenerated at each boot would break the controller's TLS
  until the ConfigMap is refreshed.
- **The country database defaults to tmpfs.** It is 5–10 MB and re-downloadable;
  rewriting it in flash on every refresh is pure wear. The cost is one download
  per boot. Set `geoip_cache_path` to persist it instead — see below.

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
| `masquerade`              | `masquerade[]`            | *(none)*               | UCI `list` of `input_device,output_device`.                 |
| `geoip_provider`          | `geoip.provider`          | *(unset: disabled)*    | `dbip`, `maxmind` or `mock`.                                |
| `geoip_license_key`       | `geoip.license_key`       | *(unset)*              | Required by `maxmind`.                                      |
| `geoip_edition`           | `geoip.edition`           | `GeoLite2-Country`     | `maxmind` only.                                             |
| `geoip_download_url`      | `geoip.download_url`      | *(provider default)*   | Mirror or offline fixture.                                  |
| `geoip_refresh_interval`  | `geoip.refresh_interval`  | `24h`                  | Go duration (`90m`, `12h`).                                 |
| `geoip_stale_after`       | `geoip.stale_after`       | `840h` dbip / `168h`   | Go duration; DB-IP updates monthly.                         |
| `geoip_cache_path`        | `geoip.cache_path`        | `/var/lib/homelab-ingress/geo.mmdb` | `maxmind`/`dbip` share one filename here.      |

Notes:

- `geoip_provider` unset means geo-blocking is off, and any Service carrying
  `homelab-ingress.vranyes.com/allowed-geos` is rejected (ADR 0003).
- `geoip_provider 'mock'` exists for e2e fixtures. The script warns, but it will
  still start if the TSV file is present.
- `dbip` data is CC BY 4.0 and requires attribution to
  [DB-IP](https://db-ip.com).

Example:

```sh
uci set homelab-ingress.config.geoip_provider='dbip'
uci set homelab-ingress.config.geoip_cache_path='/etc/homelab-ingress/geo.mmdb'  # survive reboots
uci commit homelab-ingress
/etc/init.d/homelab-ingress restart
```

## What was wrong before this change

1. **Geo-blocking could not be enabled at all.** The daemon and the YAML schema
   grew a `geoip` block, but the init script never emitted one, and the
   generated file is overwritten at each start — so editing
   `/var/etc/homelab-ingress/config.yaml` by hand was undone by the next
   `restart`, `ifup`, or reboot. Any `allowed-geos` Service failed with
   "geo-blocking is not configured" on OpenWrt.
2. **Everything was written to flash.** `-state-dir` was never passed, so
   `state.db`, the CA and (had geoip worked) the daily-rewritten country
   database all went to the `/etc` overlay.
3. **Unquoted YAML.** Values were interpolated raw. An issuer URL containing
   `#` truncated silently, a value with `: ` broke the document, and an empty
   `audience` became YAML `null` — which the daemon rejects, so procd
   restart-looped with a one-line log.
4. **Defaults duplicated three ways.** The script hardcoded
   `0.0.0.0:50051`/`50052`, the Go constants define the same, and the UCI
   sample repeated them again. They had already drifted: the UCI sample and
   `config.yaml` disagreed on `auth_audience` (`homelab-ingress` vs
   `homelab-ingress-service`).
5. **Space-joined lists.** Subjects were joined with spaces and re-split on
   whitespace, silently mangling any subject containing a space.
6. **Dangling documentation reference.** The UCI file pointed at
   `/etc/homelab-ingress/config.yaml` for the schema, but no package installed
   that file.

## Simplifications applied

- One `yaml_str` helper single-quotes and escapes every scalar; the generated
  file is `chmod 600` because it can hold a MaxMind license key.
- The script no longer owns `grpc`/`ws`/geoip defaults: unset options are
  omitted and the daemon's own defaults apply. One source of truth in Go.
- `validate_config` mirrors the daemon's checks (required auth fields,
  `masquerade` shape, provider whitelist, MaxMind key) and fails once with a
  readable message instead of a procd restart loop.
- Lists are newline-joined and split on only newlines.
- `state_dir` is configurable and the country database defaults to tmpfs.
- The YAML schema example is now actually installed.
- The translation layer has a test: `make test-init` (or
  `sh files/homelab-ingress.init.test.sh`) stubs the UCI/procd helpers, runs
  `start_service` over fixtures and asserts on the generated YAML. It is wired
  into `make test` and into CI, so the failure modes above cannot come back
  unnoticed.

## Testing

```sh
make test-init                 # the translation layer, no OpenWrt needed
sh -n files/homelab-ingress.init
shellcheck -s sh files/homelab-ingress.init
```

The test covers quote/fragment handling, newline-separated subjects, omitted
defaults, the `-state-dir` flag, the tmpfs cache default and every validation
refusal. It does not run procd itself, so a real smoke test on a device is still
`/etc/init.d/homelab-ingress restart && logread | grep homelab-ingress`.

## Simplifications still worth doing

1. **Masquerade as UCI sections instead of `"in,out"` strings.**
   UCI has no structs, so today the input/output pair is packed into one
   string and parsed with `%%`/`#`. Idiomatic UCI would be:

   ```sh
   config masquerade 'lan10-lan20'
       option input 'br-lan.10'
       option output 'br-lan.20'
   ```

   `config_foreach` walks them, the comma parser and its validation vanish, and
   each rule is named in `uci show`. Keep the old `list masquerade` as a
   deprecated fallback for one release, otherwise existing installs lose their
   masquerade rules silently.
2. **Make the websocket listener disableable.** `option ws_listen ''` cannot
   work: the YAML value becomes empty and the daemon's `applyDefaults` fills in
   the default. Either add `ws.enabled` (bool) or treat `listen: "off"` as
   disabled. Today an operator who does not want the event stream has no way to
   turn it off.
3. **Generate from a template with `sed`/`awk`?** Not recommended — the current
   explicit emitter is easier to validate and has no quoting rules beyond
   `yaml_str`.
4. **Drop the translation layer entirely** (ship a static
   `/etc/homelab-ingress/config.yaml` and let UCI only set `base_interface`).
   This is the deepest simplification, but it gives up `uci set`/`commit` as the
   documented workflow and diverges from how every other OpenWrt service is
   configured. Only worth it if the YAML schema stops changing.
