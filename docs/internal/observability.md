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

## Operational notes

- `pg_stat_statements` is loaded by the dev compose Postgres (S01) and required in prod (S37).
- GlitchTip and the OTLP collector run on bare metal in our prod topology (S37); the droplet's `/metrics` is scraped by Prometheus over WireGuard.
- Configuration documented in `docs/internal/config.md`.
