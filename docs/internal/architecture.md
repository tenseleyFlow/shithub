# Architecture

A bird's-eye view of how shithub fits together. For depth on any
one subsystem, see the linked per-area doc.

## Process model

One Go binary, three roles:

- `shithubd web` — the HTTP server. Renders the UI, serves
  `/api/v1/`, handles git over HTTPS.
- `shithubd worker` — pulls from the Postgres-backed job queue.
  Owns webhook delivery, notification fan-out, async fan-out,
  search reindex.
- `shithubd cron` — periodic chores (lifecycle sweeps, queue
  janitor, webhook purge). Triggered by a systemd timer; one-shot.

All three read the same config and the same DB. They differ only
in which `cmd/shithubd/<role>.go` they boot into.

Plus a small admin/operator CLI surface (`shithubd admin …`,
`shithubd migrate …`, `shithubd config …`, `shithubd hook …`)
that runs as one-shot subprocesses.

## Layered packages

```
                 +------------------------+
HTTP request --> | internal/web/handlers  | Per-area handlers (auth, repo, …)
                 +------------------------+
                            |
                 +------------------------+
                 | internal/web/render    | html/template + helpers
                 | internal/web/middleware| auth, csrf, ratelimit, body cap
                 +------------------------+
                            |
                 +------------------------+
                 | internal/<domain>      | repos, issues, pulls, orgs, …
                 +------------------------+
                            |
                 +------------------------+
                 | internal/auth/policy   | the only place "can X do Y" lives
                 +------------------------+
                            |
                 +------------------------+
                 | internal/<domain>/sqlc | sqlc-generated DB code
                 +------------------------+
                            |
                          Postgres
```

Cross-cutting:

- `internal/markdown` — sole renderer for user-authored markdown.
  Lint-enforced: nothing outside this package may import goldmark
  or bluemonday.
- `internal/security/{ssrf,openredirect}` — outbound HTTP defense
  + redirect validation.
- `internal/cache/lru` — generic LRU + singleflight.
- `internal/pagination/keyset` — cursor pagination with
  HMAC-signed cursors.
- `internal/webhook` — outbound webhook delivery + signing.

## Request lifecycle (web)

1. Caddy terminates TLS, forwards to `127.0.0.1:8080`, appending
   the client to `X-Forwarded-For`.
2. The chi router matches the route. Middleware stack on every
   request:
   - Request ID + access log
   - Real IP — resolves the client address by walking
     `X-Forwarded-For` rightwards, honouring it only for hops
     inside `web.trusted_proxies` (default: loopback). Everything
     below that keys on the result, so the trust list is what
     keeps rate-limit buckets from collapsing into one.
   - `OptionalUser` (resolves session cookie or anon)
   - CSRF (nosurf-derived) — enabled by default; `/api/v1/` and
     git transports are explicit exemptions
   - Rate limit (per-/24 or /48 network for anon, per-user for
     logged-in, on hot paths)
3. Auth-gated routes have `RequireUser` or `RequireSiteAdmin`
   middleware in their group.
4. The handler resolves the domain object, calls `policy.Can(...)`
   to authorize the action, executes, renders.
5. Templates render through `internal/web/render`. Markdown bodies
   pass through `internal/markdown` for sanitization.

The renderer is **process-wide**: `server.go` constructs one
`*render.Renderer` and passes it to every handler builder. One
renderer costs ~40 MB of live heap (every page holds a private copy
of the partials it reaches; html/template cannot share parse trees
across template sets), so a renderer per handler set is a
several-hundred-megabyte regression. See
`docs/internal/caching.md`, "Template renderer: one per process".

## Request lifecycle (API)

`/api/v1/*` is under a CSRF-exempt group with PAT auth:

1. `MaxBodySize` cap (256 KiB).
2. `PATAuthMiddleware` — resolves token to user; 401 on missing,
   403 on insufficient scope.
3. Per-route `RequireScope` checks the specific scope.
4. Handler runs.

## Data flow: a push

```
   git client
       |
       v   git-receive-pack POST
   web service ──> shithubd hook pre-receive ──> Postgres (auth check)
       |                          |
       |              <── decision (accept/reject)
       v                          |
   accept push,                   v
   write to /data/repos    INSERT push_events (shithub_hook role)
                                  |
                                  v
                          INSERT jobs (search reindex, notify, ...)
                                  |
                                  v
                          worker picks up job (FOR UPDATE SKIP LOCKED)
                                  |
                                  v
                          fan-out (webhooks, notifications, search)
```

The `shithub_hook` role has minimum-needed grants
(`docs/internal/db-roles.md`). The web role can do everything
the runtime needs but is intentionally separate from the migration
role used at deploy time.

## Deployment topology

shithub.sh runs on **one droplet**. Web, worker, cron, Caddy,
Postgres 16, Grafana Alloy, node_exporter, AIDE and the backup cron
jobs are all co-located on `shithub-app` (2 vCPU / 3.9 GB + a 4 GB
swapfile).

```
                +----------------------------+
   public --->  |  Caddy (TLS, rate limits)  |  :443
                +----------------------------+
                       |  127.0.0.1:8080
   +--------------------------------------------------------+
   |  droplet `shithub-app`                                 |
   |   shithubd web / worker / cron                         |
   |   Postgres 16 (localhost:5432)                         |
   |   Caddy, Grafana Alloy, node_exporter, AIDE, cron      |
   +--------------------------------------------------------+
          |                                    |
          v                                    v
   Spaces (S3): WAL archive,            Grafana Cloud
   daily dumps, LFS/blobs,              (remote_write, push-only;
   Actions logs                          no inbound port)

   3 × Actions runner droplets (SSH inbound from the app box only)
```

There is no private network and no separate database, backup, or
monitoring host. Postgres binds `localhost`; `/metrics` is served on
`127.0.0.1:8080` and read only by the local Alloy process.

The memory budget for this box, the systemd ceilings that enforce it,
and the aspirational multi-host design are in
[deploy.md](./deploy.md).

## Observability

- **Structured logs** (`internal/infra/log`) → stdout → journald.
  There is no log shipping: reading logs means SSH + `journalctl`.
- **Metrics** (Prometheus exposition) at `/metrics`, basic-auth
  gateable, scraped locally by Grafana Alloy and pushed to Grafana
  Cloud. Only the web process exposes an endpoint; the worker does
  not.
- **Tracing** (OTel HTTP) optional, sample-rate controlled, off in
  prod.
- **Error reporting** (Sentry-protocol DSN, GlitchTip-compatible).

There is **no Alertmanager and no local Prometheus**; the rules in
`deploy/monitoring/prometheus/rules.yml` are an expression catalogue,
not a running alert pipeline. See [observability.md](./observability.md)
and `deploy/monitoring/README.md`.

## What's deliberately not here

- **No microservices.** One binary per role; everything else is
  premature.
- **No background ETL pipeline.** Work that doesn't fit a job-row
  shape doesn't exist yet; if it does, add it as a new job kind
  rather than a new daemon.
- **No message bus.** Postgres LISTEN/NOTIFY suffices for the
  intra-process signaling we need.
- **No Redis.** The LRU + singleflight cache is in-process; if a
  shared cache becomes necessary, the seam is `internal/cache`
  and we'd swap in a Redis-backed implementation behind the same
  interface.

## Where to read next

- New to the code: [code-tab.md](./code-tab.md) walks how a single
  request flows end-to-end on the most-traveled page.
- New to ops: [deploy.md](./deploy.md), then
  [runbooks/](./runbooks/).
- New to security: [threat-model.md](./threat-model.md) +
  [security-checklist.md](./security-checklist.md).
