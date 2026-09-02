# `deploy/monitoring/` — not deployed

**Nothing in this directory runs in production**, with one exception
noted below. These files describe a self-hosted Prometheus + Loki +
Alertmanager + Grafana monitoring host that has never existed. They
are kept because they are the expression catalogue for the alerts we
want and the starting point if we ever outgrow Grafana Cloud — not
because they are live.

## What actually runs

shithub.sh is a single droplet. The `monitoring-client` Ansible role
installs `node_exporter` (`127.0.0.1:9100`) and **Grafana Alloy**,
which scrapes node_exporter and `shithubd web`'s
`127.0.0.1:8080/metrics` and `remote_write`s to **Grafana Cloud**.
Push-only; no inbound monitoring port; no local Prometheus, Grafana,
Loki or Alertmanager process. See `docs/internal/deploy.md` and
`docs/internal/runbooks/observability.md`.

## File by file

| Path | Status |
|---|---|
| `prometheus/prometheus.yml` | **Inert.** Scrapes six targets on a `10.50.0.0/24` WireGuard mesh that does not exist, and points `alerting.alertmanagers` at `10.50.0.10:9093`. No process reads it. |
| `prometheus/rules.yml` | **Inert.** No Prometheus loads it and there is no Alertmanager to route it. Useful as PromQL source material; see the caveats below before copying an expression. |
| `alertmanager/alertmanager.yml` | **Inert.** No Alertmanager is installed. The SMTP/webhook receivers are placeholders. |
| `loki/loki-config.yaml` | **Inert.** No Loki, and nothing ships logs — there is no Promtail and the Alloy config has no logs component. Logs are journald-only. |
| `grafana/dashboards/*.json` | **Import by hand.** These work against the Grafana Cloud stack via Dashboards → New → Import. Panel queries are mirrored in `runbooks/observability.md`. |
| `grafana/provision-actions-alerts.sh` | **Live tool.** The one thing here that talks to production. It creates/updates a Grafana-*managed* alert rule in the Cloud stack over the HTTP API. Which rules are currently provisioned is state in Grafana Cloud, not in this repo. |

## Which documented alerts therefore do not exist

Every rule in `prometheus/rules.yml` is unfired:

- `shithubd-availability`: `ShithubdWebDown`, `ShithubdWorkerDown`,
  `PostgresDown`
- `shithubd-latency`: `HighRequestLatencyP95`, `HighDBQueryRate`
- `shithubd-jobs`: `JobBacklogGrowing`, `WebhookDeliveryFailing`
- `shithubd-billing`: `BillingWebhookFailureRateHigh`,
  `BillingWebhookBacklogHigh`, `BillingWebhookFailedReceipt`,
  `BillingCheckoutFailures`, `BillingSeatDrift`,
  `BillingPastDuePrincipals`, `BillingQuotaOverage`
- `shithubd-actions`: `ActionsRunnerHeartbeatStale`,
  `ActionsRunnerIdleWithAssignedJobs`, `ActionsQueueDepthHigh`,
  `ActionsRunDurationP99Regressed`,
  `ActionsLogScrubberPossiblyMissing`
- `shithubd-backups`: `BackupOverdue`

The runbook anchors these rules reference
(`docs/internal/runbooks/incidents.md`, `backups.md`,
`stripe-billing.md`) are still correct as *procedures*; they are just
not triggered by anything. What does page today is the DigitalOcean
droplet + uptime alert set (`docs/internal/runbooks/alerts.md`).

## Caveats before copying an expression into Grafana Cloud

1. **Job labels differ.** Alloy scrapes with `job="shithubd"` and
   `job="node"`. Anything selecting `job="shithubd-web"`,
   `job="shithubd-worker"`, `job="postgres"` or `job="caddy"` matches
   nothing.
2. **The worker is not scraped at all.** `shithubd worker` starts no
   HTTP listener, so `ShithubdWorkerDown` and every `shithub_worker_*`
   expression are unsatisfiable regardless of relabelling.
3. **No Postgres exporter.** `PostgresDown` and any `pg_*` series have
   no source. WAL-archive health is checked by the hourly
   `shithub-verify-wal-archive` cron job, which logs and journals.
4. **`BackupOverdue` names a metric that never existed.** It selects
   `shithubd_backup_last_success_seconds`; our metric namespace is
   `shithub_`, and no such gauge was ever emitted. It is now real as
   `shithub_backup_last_success_seconds{job="daily"|"spaces-sync"}`,
   sourced from the heartbeat files the backup scripts write — see
   `docs/internal/runbooks/backups.md`.

## What it would take to use these files

Roughly, in order:

1. A second droplet for the monitoring host, and a private network or
   VPN between it and the app box — `deploy/ansible/roles/wireguard/`
   exists but is not run by any current inventory, and the app box has
   no `wg0`.
2. A monitoring-host play. There is none in this repo; `deploy.md`
   used to claim it lived "outside this repo" and it does not exist
   either.
3. Exporters for the targets the scrape config assumes:
   `postgres_exporter`, Caddy admin metrics, and a metrics listener in
   `shithubd worker` (a code change).
4. Relabelling `prometheus.yml` from mesh addresses to real ones and
   fixing the job names, or relabelling `rules.yml` to match Alloy's.
5. Real Alertmanager receivers — the SMTP host, credentials file and
   pager webhook in `alertmanager.yml` are all placeholders.
6. A logs pipeline if `loki-config.yaml` is to mean anything: an Alloy
   `loki.source.journal` component plus a Loki endpoint.

Until at least steps 1–4 are done, the cheaper path for any alert you
want is a Grafana-managed rule in the existing Cloud stack, added to
`grafana/provision-actions-alerts.sh` so it stays reproducible.
