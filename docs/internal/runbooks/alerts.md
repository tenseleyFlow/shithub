# Alerts

**DigitalOcean alerts are the only thing that pages.** There is no
Alertmanager and no local Prometheus in this deployment, so nothing
loads `deploy/monitoring/prometheus/rules.yml` — every alert defined
there (`ShithubdWebDown`, `ShithubdWorkerDown`, `PostgresDown`,
`HighRequestLatencyP95`, `HighDBQueryRate`, `JobBacklogGrowing`,
`WebhookDeliveryFailing`, the `shithubd-billing` and
`shithubd-actions` groups, and `BackupOverdue`) is inert. Grafana
Cloud receives the metrics and *can* host managed alert rules, but
only the ones an operator has provisioned there actually exist; see
`runbooks/observability.md` and `deploy/monitoring/README.md`.

DigitalOcean's built-in monitoring is the cheap-and-good alerting
tier. Two layers, both managed via `doctl`:

- **Resource alerts** scoped to the `shithub-app` droplet, fired when
  CPU / memory / disk / load1 cross a threshold for a sustained
  window.
- **Uptime checks** that probe the public URL from two regions and
  fire on outages, SSL-cert decay, or latency regressions.

Everything is reproducible via
`deploy/cutover/provision-do-alerts.sh` — re-running is a no-op.
The script is the source of truth for what alerts SHOULD exist;
the DO account is the source of truth for what currently fires.

## What's wired

### Droplet resource alerts

Tagged on droplet `shithub-app` (id resolves at runtime from
the droplet name in `provision-do-alerts.sh`).

| Description | Type | Threshold | Window |
|---|---|---:|---:|
| `shithub-app CPU > 80% for 10m` | `cpu` | 80% | 10m |
| `shithub-app memory > 90% for 10m` | `memory_utilization_percent` | 90% | 10m |
| `shithub-app disk > 80% for 10m` | `disk_utilization_percent` | 80% | 10m |
| `shithub-app load1 > 4 for 10m` | `load_1` | 4.0 | 10m |

Window is 10m so a single transient spike (build, AIDE re-baseline,
WAL burst) doesn't page. All four email
`wolffemf@dukes.jmu.edu` (the account-owner email; DO refuses
unverified destinations).

### Uptime check `shithub.sh`

`https://shithub.sh/healthz` probed every minute from `us_east` and
`eu_west`. Three alerts hang off it:

| Name | Type | Threshold | Period |
|---|---|---:|---:|
| `shithub.sh down (all regions)` | `down_global` | 1 region down | 5m |
| `shithub.sh SSL cert expiring < 14 days` | `ssl_expiry` | 14 days | 5m |
| `shithub.sh latency p95 > 2s` | `latency` | 2000 ms | 5m |

`down_global` deliberately requires both regions to agree before
paging — single-region noise from DO's own infrastructure is more
common than real outages and was the leading cause of false
positives on a previous setup.

`ssl_expiry < 14 days` gives a buffer to investigate Caddy's
auto-renew if it doesn't fire on its own. Caddy's renewal window
is normally 30 days before expiry, so by the time this alert
fires we're already off the happy path.

**The probe must target `/healthz`, not `/`.** `/healthz` is
registered in the CSRF-exempt group in
`internal/web/handlers/handlers.go` alongside `/static` and
`/robots.txt`: it is outside the HTML rate-limiter group, makes no
database call, and renders no template. `/` is inside that group,
so when one crawler exhausts the anonymous bucket the probe gets a
429 and DO reports the site down. That was the source of the false
"site down" emails through 2026-09-02 — 118 of 449 probe requests
in one log file were 429s. `/readyz` is not a substitute: it pings
Postgres, so a slow database turns a degradation into an outage
page.

`doctl` cannot change an uptime check's target in place. To
repoint an existing check: `doctl monitoring uptime delete
<check-id>` then re-run `provision-do-alerts.sh`, which recreates
the check and its three alerts against the `/healthz` default.

## When an alert fires

1. **Read the email.** It names the alert and the value that
   crossed.
2. **Check the droplet first**:
   ```sh
   ssh root@shithub.sh 'top -bn1 | head -20; df -h /; free -h; uptime'
   ```
3. **Common patterns**:
   - **CPU sustained > 80%** — usually `aideinit` or `aide --check`
     mid-run (12 min daily, 7 min for re-baseline). Cross-check
     against `journalctl -t shithub-aide -n 50`. If unrelated:
     `top -c -o %CPU` and identify the process.
   - **Memory > 90%** — Postgres + shithubd combined exceed budget.
     `systemctl status shithubd-web` for a dump of resident set;
     `sudo -u postgres psql -c "SELECT * FROM pg_stat_activity"` for
     long-running queries.
   - **Disk > 80%** — first check `du -sh /var/log/* /data/* /root/*
     /var/lib/postgresql/* | sort -h`. Most likely Postgres logs
     accumulating; check `log_rotation_age`.
   - **Load1 > 4** on this 2-vCPU droplet means we're saturated. If
     it's also a CPU alert, same diagnosis. If load is high but CPU
     isn't, look at I/O wait (`top` → `1` to expand cores → `wa`).
   - **down_global** — `curl -v https://shithub.sh/healthz` from
     your laptop, then `curl -v https://shithub.sh/`. A 429 on `/`
     with a 200 on `/healthz` means the anonymous rate-limit bucket
     is exhausted, not that the site is down: check
     `ratelimit.html_throttled` lines in `journalctl -u
     shithubd-web` for the offending key. Caddy log: `ssh root@shithub.sh 'tail -100
     /var/log/caddy/access.log | jq .'`. shithubd journal:
     `journalctl -u shithubd-web -n 100`.
   - **ssl_expiry** — `ssh root@shithub.sh 'systemctl status caddy;
     journalctl -u caddy -n 200 | grep -iE "renew|cert"'`. Most
     likely cause: rate-limited by Let's Encrypt or DNS
     misconfiguration after a CNAME change.
   - **latency p95 > 2s** — could be slow-query regressions, a
     wedged worker, or Caddy reverse-proxy timeouts. Start with
     `journalctl -u shithubd-web -n 200 | grep duration | sort -k1
     -nr | head`.

## Editing thresholds

Change in `deploy/cutover/provision-do-alerts.sh`, then either:

1. Delete the existing alert via `doctl monitoring alert delete
   <uuid>` and re-run the provision script (it'll create the new
   one), OR
2. Edit in place: `doctl monitoring alert update <uuid> --value
   <new>`.

For uptime alerts: `doctl monitoring uptime alert update
<check-id> <alert-id> ...`. (CLI bug: uptime check `update`
strips `--enabled`; if you mutate uptime properties, delete +
recreate via the script.)

## What's NOT here

- **shithubd-internal metrics** (request latency p95, DB pool
  saturation, job queue depth, throttle rejections). These *are*
  collected — Grafana Alloy scrapes `127.0.0.1:8080/metrics` and
  pushes to Grafana Cloud (`runbooks/observability.md`) — but no DO
  alert reads them, and a Grafana-managed rule has to be created per
  signal before any of them pages.
- **Backup failure.** Neither DO nor Grafana Cloud alerts on it. The
  backup and DR-sync scripts write a timestamped start/end/exit line
  to their logs and a heartbeat file on success
  (`/var/lib/shithub/backup-last-success`,
  `/var/lib/shithub/spaces-sync-last-success`), which the web process
  exports as `shithub_backup_last_success_seconds{job=...}`. That
  gauge reaches Grafana Cloud, so a managed rule on it is the cheapest
  way to make `BackupOverdue` real. Until then, checking is manual —
  `runbooks/backups.md`.
- **Postgres and Caddy exporters.** Not installed, so
  `pg_stat_archiver.failed_count` (the archive-failing signal) is only
  visible to the hourly `shithub-verify-wal-archive` cron job, which
  logs and journals but does not page.
- **Slack delivery.** Today everything emails. The DO API supports
  Slack webhooks; add `--slack-channels` + `--slack-urls` to the
  provision script when there's a destination.
- **Pagerduty.** Same — DO supports webhook destinations.
