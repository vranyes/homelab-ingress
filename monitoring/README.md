# Monitoring

Sample Prometheus + Grafana setup for the daemon metrics
(`metrics.listen`, default `:9102`) and the controller's
controller-runtime metrics (`--metrics-addr`, default `:8080`).

## Files

- `prometheus/scrape-example.yaml` — drop-in scrape config + `rule_files`
  (adjust `router:` / `controller:` hostnames to yours).
- `prometheus/rules.yaml` — alerting rules:
  `BackendDown`, `NftApplyErrors`, `ReconcileErrors`,
  `DHCPLeaseExpiring`, `GeoDBStale`. Tune `for:` and the geo-age
  threshold to your provider (MaxMind refreshes daily, DB-IP monthly).
- `grafana/dashboards/homelab-ingress-overview.json` — import via
  Grafana UI (Dashboards → New → Import) or provision under
  `provisioning/dashboards`. Needs a Prometheus datasource;
  the dashboard's datasource variable defaults to it.

## What to watch

- **Unhealthy backends > 0** — a backend is excluded from DNAT maps
  until probes recover (fail-open keeps the last set if *all* go down).
- **nft apply errors** — rules may be stale; check daemon logs.
- **DHCP lease TTL** near zero — renewal failing; check DHCP logs.
- **Geo DB age** climbing past refresh interval — downloader or cache
  path issue; geo-blocking keeps serving the last DB meanwhile.
