# Observability — metrics tier

Two layers of monitoring run today:

1. **DigitalOcean droplet alerts + uptime check** (see `alerts.md`).
   Tells us when the droplet is unhappy at the OS level (CPU, mem,
   disk, load, down, SSL expiry, latency from outside).
2. **Grafana Cloud free tier — Prometheus + dashboards** (this doc).
   Tells us what shithubd is doing internally: request latency,
   DB pool saturation, panics, job-queue depth, archive failures.

Layer 1 fires for "is the box alive?" Layer 2 fires for "is the
app healthy?" Different signal, different audience, both needed.

## What's wired

The `monitoring-client` ansible role runs **node_exporter**
(host metrics on `127.0.0.1:9100`) and **Grafana Alloy** (the
modern Grafana Agent rebrand). Alloy scrapes:

- `127.0.0.1:9100/metrics` — host metrics from node_exporter
- `127.0.0.1:8080/metrics` — shithubd's internal metrics

Alloy `remote_write`s both into Grafana Cloud's Prometheus
endpoint (Mimir under the hood). No inbound port on the droplet —
push-only. No Prometheus or Grafana running locally.

### Notable shithubd metrics

| Name | Type | Meaning |
|---|---|---|
| `shithub_http_in_flight` | gauge | Requests currently being served. >50 sustained = saturated. |
| `shithub_http_request_duration_seconds` | histogram | Request latency by route + method. Compute p95 from buckets. |
| `shithub_http_requests_total` | counter | All-time request count by route + method + status. Rate it for QPS. |
| `shithub_db_pool_acquired` | gauge | Active Postgres connections. Approaching `shithub_db_pool_total` = saturation. |
| `shithub_db_pool_acquire_wait_seconds_total` | counter | Cumulative wait time. Sudden derivative climb = pool too small. |
| `shithub_panics_total` | counter | Recovered panics. Should be 0 in steady state. |

## Operator setup (one-time)

Free-tier Grafana Cloud gives you ~10k active series — way more
than one shithubd droplet emits. Sign-up flow:

1. Go to https://grafana.com/auth/sign-up/create-user, pick the
   **free** plan ("Forever Free" — no card required).
2. Pick a **Stack name** (e.g. `shithub`). The portal creates
   it and lands you in the stack overview page.
3. Click **Send Metrics → Hosted Prometheus metrics**. The page
   shows the **URL**, **Username** (numeric instance ID), and a
   **Generate now** button for the password (an Access Policy token
   with `metrics:write`). Copy all three immediately — the token is
   shown once.
4. Add to `inventory/production`:
   ```ini
   grafana_cloud_prom_url=https://prometheus-prod-NN-prod-us-central-0.grafana.net/api/prom/push
   grafana_cloud_prom_user=1234567
   grafana_cloud_prom_token=glc_eyJrIjoi...   # the token from step 3
   ```
5. `ansible-playbook -i inventory/production deploy/ansible/site.yml -t monitoring`
   (or just rerun site.yml — the role is idempotent).

Within a minute, metrics start landing. From the Cloud portal:

- **Explore** tab → datasource = your Prometheus → query
  `up{job=~"node|shithubd"}` — both should show 1.
- Sample queries to confirm shithubd telemetry:
  - `sum(rate(shithub_http_requests_total[5m]))` — overall QPS
  - `histogram_quantile(0.95, sum by (le, route) (rate(shithub_http_request_duration_seconds_bucket[5m])))` — p95 latency by route
  - `shithub_db_pool_acquired / shithub_db_pool_total` — pool utilization
  - `rate(shithub_panics_total[5m]) > 0` — recovered panics

## Building a starter dashboard

Cloud → Dashboards → New → import. Use the panels below as a
spine. (We're not committing the dashboard JSON to the repo yet
because Grafana's UUIDs are stack-specific; doc the queries and
let the operator import once.)

| Panel | Query |
|---|---|
| QPS | `sum(rate(shithub_http_requests_total[5m]))` |
| p95 latency by route | `histogram_quantile(0.95, sum by (le, route) (rate(shithub_http_request_duration_seconds_bucket[5m])))` |
| Error rate | `sum(rate(shithub_http_requests_total{status=~"5.."}[5m])) / sum(rate(shithub_http_requests_total[5m]))` |
| In-flight requests | `shithub_http_in_flight` |
| DB pool utilization | `shithub_db_pool_acquired / shithub_db_pool_total` |
| Pool wait derivative | `rate(shithub_db_pool_acquire_wait_seconds_total[5m])` |
| Panics | `rate(shithub_panics_total[5m])` |
| Host CPU | `100 - avg by (instance) (irate(node_cpu_seconds_total{mode="idle"}[5m]) * 100)` |
| Host memory used | `(node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes) / node_memory_MemTotal_bytes` |
| Disk free | `node_filesystem_avail_bytes{mountpoint="/"}` |

## Alerting

Three places to set alert rules. Pick one:

- **Grafana Cloud → Alerts → Alert rules.** UI-first; fires to
  email/Slack/PagerDuty via Cloud's contact points.
- **Alertmanager via remote_write.** Out of scope for the free
  tier without their managed contact points.
- **Skip Grafana alerts entirely; use the DO alerts (`alerts.md`)
  for operational pages and read shithubd metrics manually.**

Suggested first three alerts (Cloud UI):

| Name | Query | Threshold | Reason |
|---|---|---|---|
| shithubd p95 latency high | p95 latency query above | `> 1s` for 5m | UI feels slow before users notice |
| DB pool saturation | utilization query | `> 0.8` for 10m | Pool too small or query regression |
| Recovered panic in last 5m | panics rate | `> 0` for 1m | Bug ticket worth |

## When metrics stop landing

```sh
ssh root@shithub.sh '
  systemctl status alloy --no-pager
  journalctl -u alloy -n 30 --no-pager | tail -20
  curl -fsS http://127.0.0.1:8080/metrics | head -3
  curl -fsS http://127.0.0.1:9100/metrics | head -3
'
```

Common failures:

- **401 Unauthorized in alloy logs** — token expired or
  copy-pasted wrong. Mint a fresh one in the Cloud portal,
  update inventory, re-run the role.
- **node_exporter not listening on 9100** — `apt reinstall
  prometheus-node-exporter`, check journal.
- **shithubd not exposing /metrics** — `SHITHUB_METRICS__ENABLED=true`
  in web.env (the default), restart shithubd-web.
- **Free-tier limit hit** — Cloud portal shows "Active series" at
  cap. Drop high-cardinality labels via `metric_relabel_configs` in
  alloy-config.river.j2.

## Why Grafana Cloud over self-hosted Prometheus

- Self-hosted needs Prom + Grafana on the droplet → ~500 MB RAM,
  storage for time-series, an open inbound port (or VPN), TLS for
  the Grafana UI, an OAuth proxy for auth, and we maintain the
  upgrade path.
- Free Grafana Cloud is enough for our scale, gives us managed
  backup of metrics + dashboards + alerts, and the data leaves
  the droplet (so a droplet outage doesn't kill its own
  observability).
- Trade-off: cardinality cap. Once we approach 10k active series,
  evaluate either paid Cloud (~$8/mo for 10× the cap) or self-host.
