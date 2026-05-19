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
| `shithub_actions_queue_depth` | gauge | Queued Actions runs/jobs. Sustained job depth means runners cannot keep up. |
| `shithub_actions_active` | gauge | Running Actions runs/jobs. Use with capacity to distinguish slow jobs from lack of runners. |
| `shithub_actions_runner_heartbeat_age_seconds` | gauge | Seconds since each runner heartbeat. >60s sustained means the runner is stale. |
| `shithub_actions_runner_active_jobs` | gauge | Running jobs assigned to each runner. Nonzero on an idle runner indicates a stale assignment wedge. |
| `shithub_actions_run_duration_seconds` | histogram | Terminal Actions run duration by event and conclusion. |
| `shithub_actions_log_chunk_bytes_total` | counter | Accepted Actions log bytes, used for throughput and scrubber health alerts. |

## Operator setup (one-time)

Free-tier Grafana Cloud gives you ~10k active series — way more
than one shithubd droplet emits. Sign-up flow:

1. Go to https://grafana.com/auth/sign-up/create-user. As of 2026
   the default flow drops you into a 14-day "Unlimited usage trial"
   — that's fine; without a card on file it auto-reverts to the
   Forever Free tier (~10k active series) when the trial ends.
2. Pick a **Stack name** (e.g. `shithub`). The portal creates the
   stack at `<name>.grafana.net` and lands you in the in-app
   "Get started" guide.
3. Get the Prometheus details. The in-app guide has drifted; the
   reliable path is via the org-level Cloud portal:
   - In the right column of the Get started page, **Organization
     details** → click **Cloud portal**, OR navigate directly to
     `https://grafana.com/orgs/<your-org>`.
   - Click **Details** on the **Prometheus** tile.
   - The page shows the **Remote Write Endpoint** (full URL),
     **Username / Instance ID** (numeric), and a **Generate now**
     link for the **Password / API Token** (a `glc_…` Access Policy
     token with `metrics:write`). Copy the token immediately — it's
     shown once.
4. Add to `inventory/production`. Use the exact Remote Write Endpoint
   shown on the page — the host (prod-NN, region) differs per tenant:
   ```ini
   grafana_cloud_prom_url=https://prometheus-prod-XX-prod-REGION.grafana.net/api/prom/push
   grafana_cloud_prom_user=1234567
   grafana_cloud_prom_token=glc_eyJrIjoi...   # the token from step 3
   ```
5. `ansible-playbook -i inventory/production deploy/ansible/site.yml -t monitoring`
   (or just rerun site.yml — the role is idempotent).

### If you don't have ansible installed (manual fallback)

The `monitoring-client` role's actions are simple enough to apply
by hand over SSH. Use this if you're trying to bolt monitoring
onto an existing droplet and don't want to risk the full play.

```sh
ssh root@shithub.sh '
  # node_exporter
  apt-get install -y prometheus-node-exporter
  systemctl enable --now prometheus-node-exporter

  # Grafana apt repo + alloy
  mkdir -p /etc/apt/keyrings
  curl -fsS https://apt.grafana.com/gpg.key -o /etc/apt/keyrings/grafana.gpg.key
  chmod 0644 /etc/apt/keyrings/grafana.gpg.key
  echo "deb [signed-by=/etc/apt/keyrings/grafana.gpg.key] https://apt.grafana.com stable main" \
    > /etc/apt/sources.list.d/grafana.list
  apt-get update && apt-get install -y alloy

  # Credentials env (mode 0640, root:alloy)
  mkdir -p /etc/alloy && chown root:alloy /etc/alloy && chmod 0750 /etc/alloy
  install -o root -g alloy -m 0640 /dev/stdin /etc/alloy/credentials.env <<EOF
GRAFANA_CLOUD_PROM_URL=<your URL>
GRAFANA_CLOUD_PROM_USER=<your numeric tenant id>
GRAFANA_CLOUD_PROM_TOKEN=<your glc_… token>
EOF

  # systemd drop-in to source the credentials
  mkdir -p /etc/systemd/system/alloy.service.d
  cat > /etc/systemd/system/alloy.service.d/shithub.conf <<EOF
[Service]
EnvironmentFile=/etc/alloy/credentials.env
EOF
  systemctl daemon-reload
'
```

Then copy the rendered `alloy-config.river.j2` (substitute the
`{{ ansible_hostname }}` template var with the real hostname) to
`/etc/alloy/config.alloy` (mode 0644 root:alloy) and:

```sh
ssh root@shithub.sh 'systemctl enable --now alloy'
```

The token is the only secret; do NOT pipe it through your shell
history. The `install … /dev/stdin … <<EOF` form above keeps it
out of `ps`/argv, but you'll still want to clear shell history
after.

Within a minute, metrics start landing. From the Cloud portal:

- **Explore** tab → datasource = your Prometheus → query
  `up{job=~"node|shithubd"}` — both should show 1.
- Sample queries to confirm shithubd telemetry:
  - `sum(rate(shithub_http_requests_total[5m]))` — overall QPS
  - `histogram_quantile(0.95, sum by (le, route) (rate(shithub_http_request_duration_seconds_bucket[5m])))` — p95 latency by route
  - `shithub_db_pool_acquired / shithub_db_pool_total` — pool utilization
  - `rate(shithub_panics_total[5m]) > 0` — recovered panics

## Building a starter dashboard

Cloud → Dashboards → New → import. Dashboard JSON is committed under:

```text
deploy/monitoring/grafana/dashboards/shithubd-overview.json
deploy/monitoring/grafana/dashboards/actions.json
```

Use the panels below as fallback queries if the dashboard import UI drifts or
you need to rebuild by hand.

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

### Billing panels

Add these once paid plans are enabled:

| Panel | Query |
|---|---|
| Billing webhook outcomes | `sum(rate(shithub_billing_webhook_events_total[15m])) by (event_type, result)` |
| Billing webhook backlog | `shithub_billing_webhook_backlog` |
| Checkout failures | `sum(increase(shithub_billing_checkout_sessions_total{result="failure"}[10m])) by (subject_kind)` |
| Billing Portal failures | `sum(increase(shithub_billing_portal_sessions_total{result="failure"}[10m])) by (subject_kind)` |
| Team seat drift | `shithub_billing_org_seat_drift` |
| Past-due accounts | `shithub_billing_past_due_principals` |
| Quota overages | `shithub_billing_quota_overage_orgs` |

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

Billing alert rules are committed in
`deploy/monitoring/prometheus/rules.yml` under `shithubd-billing`.
Load them before live billing launch. The corresponding response steps
live in [`stripe-billing.md`](./stripe-billing.md#billing-alerts).

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
- **`up{job="shithubd"} = 0` while `up{job="node"} = 1`** — alloy can
  reach shithubd (`scrape_samples_scraped > 0`) but the parser fails
  with `expected a valid start token, got "\x1f"`. Root cause: alloy
  advertised `Accept-Encoding: gzip`, shithubd correctly returned
  gzipped bytes with `Content-Encoding: gzip`, but alloy's
  Prometheus scraper parsed the raw bytes pre-gunzip. Fix is already
  in the template (`enable_compression = false` on the shithubd
  scrape job). If you see this on a hand-rolled config, add that
  attribute and `systemctl restart alloy`. Verify via
  `curl http://127.0.0.1:12345/api/v0/web/components/prometheus.scrape.shithubd`
  → look for `health: up`.

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
