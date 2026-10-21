# ADR 0006: DB-IP Lite as a no-key country database provider

**Date:** 2026-10-20
**Status:** Accepted
**Amends:** ADR 0003 (the optional-database rule still holds; the provider is no
longer necessarily MaxMind).

## Context

ADR 0003 made the MaxMind block optional and rejected geo requests when no
database was configured. GeoLite2 itself is free, but it requires a MaxMind
account and a license key, which is friction for a homelab deployment and
another secret to rotate.

Several free country databases exist. The important constraint for this
codebase is that the daemon only needs ISO-3166 alpha-2 codes per IPv4 range;
`geo.Manager` and nftables consume country→CIDR lists, not packet-time lookups.
That makes any country-level MMDB a drop-in fit.

## Decision

Support two real providers behind an explicit `geoip.provider` setting, with
`mock` retained for e2e:

| provider | key | source | format | cadence |
|---|---|---|---|---|
| `maxmind` | required | GeoLite2 Country | `.tar.gz` containing an MMDB | daily refresh |
| `dbip` | none | DB-IP IP-to-Country Lite | `.mmdb.gz` (gzip of the raw MMDB) | monthly |
| `mock` | none | local TSV fixture | TSV | n/a |

### Why DB-IP Lite

- No account or license key.
- Standard MMDB 2.0 format with `country.iso_code`, the exact field
  `openMMDB` already reads; the reader is unchanged.
- ~8 MB, one release per calendar month.
- Licensed CC BY 4.0. Attribution to DB-IP is required; the daemon logs
  `geo data by DB-IP (https://db-ip.com)` at startup when selected, and the
  docs/config sample carry the link.

### Config

The `maxmind` block is renamed to `geoip` while the feature is unreleased:

```yaml
geoip:
  provider: dbip            # maxmind | dbip | mock; omit to disable
  # license_key: ...        # required only for provider: maxmind
  # download_url: ...       # optional mirror/offline override
  refresh_interval: 24h
  stale_after: 840h         # 35d for the monthly DB-IP release
  cache_path: /var/lib/homelab-ingress/dbip-country-lite.mmdb
```

A bare `license_key` with no `provider` still means MaxMind, and
`license_key: mock` still selects the e2e fixture, so no existing config breaks.

### Refresh and failure behaviour

- DB-IP publishes one file per UTC calendar month. The manager skips the
  download when the cache mtime is in the current month; on the first tick of a
  new month it fetches the new URL. A 404 before DB-IP publishes keeps the old
  cache and is retried on the next daily tick.
- A custom `download_url` bypasses the month check (the operator owns cadence).
- MaxMind keeps its unconditional daily refresh and 7-day stale window.
- Both providers download to a temp file, fsync, sanity-check the MMDB
  metadata marker, and rename atomically. A failed download never clobbers a
  good cache.

## Consequences

- No license key is needed for the common path; geo-blocking is still opt-in
  (`provider` must be set), so a deployment that does not use it neither
  downloads data nor changes behaviour.
- DB-IP Lite is less accurate than MaxMind (DB-IP publishes an accuracy index
  of 81 for Lite vs 93 for its commercial database). Country-level allow-lists
  tolerate that; operators who need the best accuracy can still choose
  `maxmind`.
- The Go package and config block are named `geoip`, not `maxmind`, because the
  backend is now pluggable.
- The e2e suite continues to use the deterministic TSV mock, so it does not
  depend on either provider's network availability.
