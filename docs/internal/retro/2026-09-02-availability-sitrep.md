# Availability sitrep and campaign — 2026-09-02

Status: **active campaign**. This file is the directive for the
availability work; update the checklists as items land.

## TL;DR

shithub.sh runs on one 2 vCPU / 3.9 GB droplet (`shithub-app`,
24.199.108.81) with web, worker, cron, Caddy, local Postgres, Alloy,
AIDE, and the backup jobs all co-located, **no swap and no memory
ceiling on any unit**. The outages are kernel OOM events, not
traffic. Three things stack up:

1. `shithubd web` holds ~0.7 GB of *static* live heap because the
   web wiring constructs **eight** `render.Renderer`s, each parsing
   all 179 page templates with every partial cloned in. Measured
   locally on trunk: 83 MB per renderer, 664 MB for eight. With
   `GOGC=100` the GC target is ~1.46 GB and RSS sits at 1.3–1.85 GB.
2. The hourly cross-region backup mirror (`shithub-spaces-sync`,
   rclone with `--fast-list` over a 161k-object WAL bucket) runs
   **on the app box** for ~28 min of every hour at 1.0–1.6 GB RSS.
   The script's own header says it must not run there.
3. A 03:17–04:45 cron pile-up: `pg_dump`, rclone, **two** concurrent
   AIDE scans (`dailyaidecheck.timer` was never disabled alongside
   the Ansible cron entry), `shithubd-cron`, and a nightly
   `lifecycle:sweep` retry loop.

Result: baseline ~2.4 GB + AIDE ~0.5 GB + rclone 1.0–1.6 GB > 3.9 GB.
`sar` at 04:30 UTC today: 47 MB available, load 21, 26 tasks in D
state, workers timing out on SCRAM auth to Postgres on loopback.
Nine OOM kills in the last week, alternating between `shithubd` and
`rclone` (`global_oom`, so the kernel picks the largest process).

Separately, **all anonymous visitors share one rate-limit bucket.**
`server.go:131` wires `middleware.RealIP(RealIPConfig{})` with no
trusted proxies, so behind Caddy every anonymous request keys on
`127.0.0.1`. The HTML limiter's anonymous tier is 60 hits / 60 s
per key, Meta's crawler burns it continuously, and every other
anonymous visitor gets 429 — including the DigitalOcean uptime
probe (118 of 449 probe requests in the last log file), which is
the source of the false "site down" emails. 23% of all responses
are 429.

## Evidence (collected 2026-09-02, read-only)

| Signal | Value |
|---|---|
| Box | 2 vCPU, 3915 MB, no swap, uptime 115 d |
| `shithubd web` | 1.33 GB RSS, 921 MB heap_alloc, next_gc 1.46 GB, 18 goroutines, 21 fds, NRestarts=16 |
| OOM kills, 7 days | shithubd ×5 (1.57–1.85 GB), rclone ×4 (1.06–1.62 GB) |
| Alloy | 317 MB RSS, scraping an 11,747-series `/metrics` |
| Postgres | 447 MB aggregate, 19 conns, shared_buffers 128 MB (Debian default; `postgresql.conf.j2` never applied) |
| DB | 988 MB, of which 881 MB is `repo_traffic_paths` + `repo_traffic_uniques`, unpruned since 2026-05-18 |
| Traffic | 29% runner heartbeats (3 × every 5 s), 19% `meta-externalagent` walking every commit SHA / blob / raw; 23% of all responses are 429 |
| Git | 14 repos, 1.6 GB, zero git subprocesses at sample time |
| Runners | all 3 idle, heartbeating, queue drained; SSH from laptop blocked by cloud firewall (reachable from app box) |
| Deployed build | `v0.1.0-1767-g1dc532f6` = origin/trunk head (2026-05-29) |

Full agent reports (not committed): codebase assessment, prod
forensics, infra assessment in the session scratchpad.

### Root causes, ranked

| # | Cause | Confidence | Fix class |
|---|---|---|---|
| 1 | 8× template renderer duplication (`internal/web/*_wiring.go`, `handlers.go:212`) → ~660 MB static heap | very high (measured) | code |
| 2 | rclone `--fast-list` hourly on app box (`deploy/spaces/sync-cross-region.sh`, `roles/backup`) | very high | deploy |
| 3 | No `MemoryMax`/`MemoryHigh`/`GOMEMLIMIT`/swap anywhere | very high | deploy |
| 4 | Cron pile-up + duplicate AIDE timer | very high | deploy |
| 5 | Metrics label leak: `route := r.URL.Path` fallback for unmatched routes (`internal/web/middleware/metrics.go:30`) → immortal series per scanner probe | high (small memory, large Alloy/Grafana cost) | code |
| 6 | Code tab forks `git log -1 -- <path>` per tree entry, plus `rev-list --count`, `git log -n 500`, `ls-tree -r` per repo home view; crawlers hit these anonymously | high (CPU) | code |
| 7 | `repo_traffic_*` has no retention job; grows ~30k rows/day/table from crawler paths | high | code |
| 8 | `RealIP` has no trusted proxies (`internal/web/server.go:131`) → all anon traffic keys on `127.0.0.1`; HTML/API/signup limiters collapse to one shared bucket | very high | code |
| 9 | Worker opens `MaxConns = workers+2` before `workers` defaults to 4 → 4 workers on a 2-conn pool (`cmd/shithubd/worker.go:76-84`, `internal/worker/pool.go:49`); `SHITHUB_WORKERS` unset in prod | high (latent; queue currently drained) | code/deploy |
| 10 | `actionsobserver` runs `sum(octet_length(chunk))` over the whole log-chunk table every 15 s | medium | code |
| 11 | Actions SSE log stream pins a pool connection for stream lifetime against a 10-conn pool | medium | code |
| 12 | No pprof endpoint; no `pg_stat_statements` | — (blocks diagnosis) | code/deploy |

### Ruled out

Goroutine/fd leaks (18 / 21), git subprocess storms (none observed),
disk (2% of /data), Postgres tuning, job-queue backlog (0 pending),
runner outage (all healthy), traffic volume (flat ~950 ok / 10 min).

### Repo state note

The local checkout branch `s41i/runner-dogfood-smoke` is **1,307
commits behind origin/trunk**. All fixes must branch from
`origin/trunk`. Every push to trunk auto-deploys after CI
(`.github/workflows/deploy.yml`), restarting web and worker.

## Campaign

### Phase 0 — stop the bleeding (operator, on the box, no deploy)

The first four items are now also mirrored into Ansible (base +
backup roles, `deploy/spaces/sync-cross-region.sh`), so the next
`make deploy` re-applies them instead of reverting the hand edits.
The verification items below are still the operator's.

- [ ] Add 4 GB swapfile on `/data`, `vm.swappiness=10`, persist in fstab
- [ ] Disable `dailyaidecheck.timer` (keep `shithub-aide-check` cron)
- [ ] Move `shithub-spaces-sync` from hourly to every 6 h, off the
      03:00–05:00 window; drop `--fast-list` for the WAL bucket; add a
      flock so runs cannot overlap
- [ ] Move `shithub-aide-check` to 06:00, `shithubd-cron.timer` to 05:15
- [ ] Confirm DR bucket integrity with `rclone check --size-only --one-way`
- [ ] Verify next morning: no OOM in `journalctl -k`, `sar -r` peak < 75%

### Phase 1 — bound the process (PR, deploys automatically)

- [x] `deploy/systemd/shithubd-web.service`: `Environment=GOMEMLIMIT=1200MiB`,
      `MemoryHigh=1600M`, `MemoryMax=2000M`, `OOMScoreAdjust=-500`
- [x] `deploy/systemd/shithubd-worker.service`: `MemoryHigh=768M`,
      `MemoryMax=1024M` (cgroup-wide, so it also covers git forked by jobs)
- [x] `worker.env.j2`: set `SHITHUB_WORKERS=4` **and** fix `MaxConns`
      computation to use the resolved worker count
      (`resolveWorkerCount` in `cmd/shithubd/worker.go`)
- [x] pprof on loopback — `web.pprof_addr` starts a separate
      `http.Server` serving only `net/http/pprof`, refuses any
      non-loopback address, and is never mounted on the app router.
      Heap-profile procedure in `runbooks/observability.md`.
- [x] Tests: unit test for worker pool sizing; systemd unit lint in CI
      (`scripts/lint-systemd-units.sh`)
- [x] `shithubd-cron.timer` moved to 05:15 UTC, out of the backup window

### Phase 2 — cut the static heap (PR)

- [x] Construct one shared `render.Renderer` in `server.go` and pass it
      to every handler builder (target: ≤ 100 MB template heap; measure
      with the probe test) — **664 MB → 41 MB**
- [x] Follow-up: stop cloning all 26 partials into every page — done by
      pruning to each page's transitively-referenced set, **83 MB → 41 MB
      per renderer**; output parity checked over all 153 pages
- [x] Tests: memory probe as a benchmark with a ceiling assertion
      (`internal/web/renderer_heap_test.go`, 150 MB ceiling + an AST
      scan that fails on a second `render.New` in package web)
- [ ] Remaining: `_layout.html` is 32 KB of the ~54 KB every page still
      pulls in, in one monolithic `{{ define "layout" }}`. Splitting it
      is worth roughly another 20 MB but is a template refactor.

### Phase 3 — stop the crawler amplification (PR)

- [x] `metrics.go`: label unmatched routes as `"unmatched"`; add test
- [x] Add `web.trusted_proxies` config (default `127.0.0.0/8, ::1/128`)
      and pass it to `middleware.RealIP` so X-Forwarded-For from Caddy
      is honoured; test with a forged XFF from an untrusted address
- [x] Exempt `/healthz` (or the DO probe UA) from the HTML limiter —
      `/healthz` was already outside the limiter group; the DO uptime
      check probed `/`, so it was repointed at `/healthz` in
      `provision-do-alerts.sh` (needs a delete + recreate on the
      account, see `runbooks/alerts.md`)
- [x] Key the anonymous HTML tier by `/24` for repo history/blob/raw
      routes (Meta rotates within `57.141.2.0/24`) — applied to the
      whole anonymous tier, not just those routes
- [x] Retention job for `repo_traffic_paths` / `repo_traffic_uniques`
      (plus `_referrers`) — `traffic:purge`, nightly from
      `shithubd-cron.service`, 30-day window rather than the UI's 14 so a
      purge can never truncate the chart; `repo_traffic_daily` keeps 400
      days. Deletes run in 5k-row batches, capped per run, so the first
      pass over the 2.5 M-row backlog never holds a long transaction —
      which is also why the backfill is the job's first run and not a
      bulk DELETE in a migration. 0129 adds the `day` indexes the purge
      needs (`repo_traffic_uniques` reuses its existing `created_at`
      index). See `docs/internal/repository-insights.md`.
- [ ] Cache per-entry last-commit for the code tab (single
      `git log --name-only` walk, or an LRU keyed by tree OID) and
      cache `rev-list --count` / recursive `ls-tree` per head OID
- [x] `actionsobserver`: the `octet_length` sum now runs every 5 min on its
      own cadence; the count and queue-depth gauges stay at 15 s

### Phase 4 — observability and docs

- [ ] Enable `pg_stat_statements`
- [ ] `docs/internal/observability.md` / `deploy.md`: replace the
      WireGuard + monitoring-droplet story with the real Alloy →
      Grafana Cloud pipeline; note there is no Alertmanager, so the
      runbook backup alerts cannot fire
- [ ] `runbooks/alerts.md:77`: box is 2 vCPU, not 4
- [ ] Backup scripts log a timestamped success line; both logs are
      0 bytes since May 10
- [ ] Decide `postgresql.conf.j2`: revise for a shared 4 GB box
      (shared_buffers 256 MB, work_mem 4 MB) and track it in
      `check-droplet-drift.sh`, or delete it
- [ ] Add `/etc/postgresql/16/main/postgresql.conf` and the Stripe
      hand-edit in `web.env` to drift tracking

### Operator-only items (need account access)

- [ ] `doctl` token is revoked/expired (401); rotate it
- [ ] Runner firewall: add current laptop egress IP to the SSH rule
      (interim: `ssh -J root@24.199.108.81 root@<runner>`)
- [ ] Spaces access key is plaintext in both env files; rotate if the
      exposure set is wider than intended
- [ ] Consider resizing the droplet to 8 GB if Phase 0–2 do not hold
      peak memory under 75%

## Verification targets

- `journalctl -k | grep -i oom` empty for 7 days
- `sar -r` daily peak `%memused` < 75%
- `process_resident_memory_bytes{job="shithubd"}` < 600 MB after Phase 2
- `/metrics` series count < 1,000 after Phase 3
- DO uptime probe: zero 429 responses in Caddy access log
- `lifecycle:sweep` completes nightly without retries
