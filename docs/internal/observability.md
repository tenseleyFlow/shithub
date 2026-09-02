# Observability

shithub ships four sinks: structured logging, Prometheus metrics, OpenTelemetry tracing, and a Sentry-protocol error reporter. All four are governed by the layered config loader (`docs/internal/config.md`) and degrade to no-ops when not configured.

## Logging

- Library: `log/slog` (stdlib).
- Format: `text` (default in dev, human-friendly key=value) or `json` (default in prod, one object per line).
- Level: `debug | info | warn | error` (config: `log.level`).
- Standard fields on every line:
  - `time`, `level`, `msg`
  - `request_id` — when a request is in flight (set by the request_id middleware).
  - `user_id` — when known (post-S05).
  - `component` — set by the package emitting the line.
  - `error`, `stack` — on error-level lines, where applicable.
- **Redaction.** `internal/infra/log` wraps the chosen handler so each record passes through a redactor before output:
  - Attribute keys matching `password|pass|secret|key|token|authorization|dsn|otpauth` are redacted to `***`.
  - Attribute string values containing `shithub_pat_`, `otpauth://`, `Bearer `, or `Basic ` are redacted to `***` regardless of the key.
  - Tested in `internal/infra/log/log_test.go`.

## Metrics

- Library: `github.com/prometheus/client_golang`.
- Endpoint: `GET /metrics` (Prometheus text exposition).
- Access control: HTTP Basic auth when `metrics.basic_auth_user`/`metrics.basic_auth_pass` are set; otherwise unauthenticated. S35 will tighten further (IP allow-list + per-deployment policy).
- Standard metrics:
  - `shithub_http_requests_total{route,method,status}` (counter)
  - `shithub_http_request_duration_seconds{route,method}` (histogram, exponential buckets)
  - `shithub_http_in_flight` (gauge)
  - `shithub_panics_total` (counter, incremented by the recover middleware)
  - `shithub_db_pool_acquired`, `shithub_db_pool_idle`, `shithub_db_pool_total` (gauges)
  - `shithub_db_pool_acquire_wait_seconds_total` (counter)
  - `shithub_billing_checkout_sessions_total{subject_kind,result}` (counter)
  - `shithub_billing_portal_sessions_total{subject_kind,result}` (counter)
  - `shithub_billing_webhook_events_total{event_type,result}` (counter)
  - `shithub_billing_webhook_backlog{state}` (gauge)
  - `shithub_billing_past_due_principals{subject_kind}` (gauge)
  - `shithub_billing_org_seat_drift` (gauge)
  - `shithub_billing_quota_overage_orgs{quota}` (gauge)
  - `shithub_backup_last_success_seconds{job}` (gauge; read at scrape
    time from the heartbeat file each backup cron job writes on
    success. The series is **absent** until a job has succeeded once
    on that host, so alert on `absent()` as well as on age.)
  - Standard Go runtime + process metrics (registered automatically).
- **Cardinality discipline.** Route labels come from chi's `RoutePattern()` so we get `/owner/{repo}` instead of per-repo concrete paths. Never label by `user_id` or `repo_id`.
- Per-domain metrics (added in later sprints) MUST register against `metrics.Registry` so a single `/metrics` scrape sees everything.

Billing webhook event-type labels are normalized to the supported
Stripe event set plus `other` and `unknown`; never label by Stripe
customer, subscription, invoice, org, repo, or user ids.

## Tracing

- Library: OpenTelemetry SDK with the OTLP-HTTP exporter.
- Disabled by default. To enable:
  ```toml
  [tracing]
  enabled = true
  endpoint = "http://otel-collector.bare-metal:4318"
  sample_rate = 0.05
  service_name = "shithubd"
  ```
- The HTTP middleware (`tracing.Middleware`) emits one span per request when enabled.
- pgx tracer hook lands when domain queries arrive (post-S05); the bare DB Open does not yet attach it.
- Sample rate is parent-based with `TraceIDRatioBased(sample_rate)` — incoming traces with parent context honor the parent decision.

## Error reporting

- Library: `github.com/getsentry/sentry-go`. The wire format is Sentry-protocol-compatible, so a self-hosted GlitchTip works as a drop-in receiver (the lean per S03's sprint spec).
- DSN comes from `error_reporting.dsn`. When empty, the package is a no-op.
- Two integration points:
  1. **Recover middleware** calls `errrep.CapturePanic(recovered, requestID)` for every recovered panic. The request_id is attached as a tag for forensic correlation with logs.
  2. **`errrep.SlogHandler`** wraps the slog handler so any record at error level is also reported as a Sentry event. Tags: `request_id`, `user_id`, `component`, `route`. Other attrs land under the `shithub` context.
- Flush: `errrep.Init` returns a `flush(ctx)` callback the server invokes on shutdown (drains queued events).

## Request ID flow

The `request_id` is the correlation key tying logs, metrics, traces, and error reports together:

1. `middleware.RequestID` accepts an inbound `X-Request-Id` if it matches a strict whitelist; otherwise generates a 16-byte hex value.
2. The id is stored in the request context and echoed in the response `X-Request-Id` header.
3. `middleware.AccessLog` includes it as `request_id` on the per-request log line.
4. `middleware.Recover` includes it on panic logs and on the Sentry/GlitchTip event.
5. The styled error pages (`errors/{404,403,429,500}.html`) display it for end-user support reference.

## How this reaches an operator in production

shithub.sh is a single droplet (`docs/internal/deploy.md`). The
pipeline is:

```
shithubd web  /metrics (127.0.0.1:8080) --.
                                          +--> Grafana Alloy --remote_write--> Grafana Cloud
node_exporter (127.0.0.1:9100) ----------'                                     (Prometheus/Mimir)
```

- Alloy is installed by the `monitoring-client` Ansible role. It
  scrapes with job labels `shithubd` and `node`, and pushes; **no
  inbound monitoring port is open on the droplet.**
- **The worker exposes no `/metrics` endpoint.** `shithubd worker`
  starts no HTTP listener, so `shithub_worker_*` and any other
  worker-side series are collected by nothing. Worker health has to be
  read from `journalctl -u shithubd-worker` or from DB state.
- **There is no log pipeline.** No Promtail, no Loki, no Alloy logs
  component. Logs live in journald on the box and nowhere else, so
  log-based correlation (including the `request_id` flow below) is an
  SSH-and-grep exercise.
- **There is no Alertmanager and no local Prometheus.** Nothing loads
  `deploy/monitoring/prometheus/rules.yml`. Every alert defined there
  — `ShithubdWebDown`, `ShithubdWorkerDown`, `PostgresDown`,
  `HighRequestLatencyP95`, `HighDBQueryRate`, `JobBacklogGrowing`,
  `WebhookDeliveryFailing`, the seven `shithubd-billing` alerts, the
  five `shithubd-actions` alerts, and `BackupOverdue` — **does not
  fire.** Several of them could not fire even with a Prometheus
  attached, because they select on job labels (`shithubd-web`,
  `shithubd-worker`, `postgres`, `caddy`) that this deployment never
  emits.
- What *does* page today: DigitalOcean droplet resource alerts and
  the uptime check (`runbooks/alerts.md`), plus any Grafana-managed
  alert rule provisioned into the Cloud stack by hand or by
  `deploy/monitoring/grafana/provision-actions-alerts.sh`.

Operator setup, dashboard queries, pprof procedure and the
"metrics stopped landing" checklist: `runbooks/observability.md`.
Why the committed monitoring configs are inert:
`deploy/monitoring/README.md`.

## Operational notes

- `pg_stat_statements` is loaded by the dev compose Postgres (S01).
  It is **not** installed on the production box; the Ansible
  `postgresql.conf.j2` that would load it has never been applied
  (`docs/internal/db.md`).
- GlitchTip and the OTLP collector are not deployed;
  `error_reporting.dsn` and `tracing.enabled` are unset in prod, so
  both packages are no-ops there.
- Configuration documented in `docs/internal/config.md`.
