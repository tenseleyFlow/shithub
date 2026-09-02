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
The verification items below are still the operator's. **Every
unticked box in this phase is restated with exact commands in
[Operator to-do](#operator-to-do) at the end of this file.**

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
- [x] Cache per-entry last-commit for the code tab — one streamed
      `git log --name-only` walk per directory
      (`repogit.EntryLastCommits`) replacing one `git log -1` per
      entry, plus an OID-keyed LRU
      (`internal/web/handlers/repo/treecache`) over that, the
      `rev-list --count`, the `ls-tree -r` language aggregate and the
      `log -n 500` contributor tally. Measured on the handler test
      fixture: a cold root-tree render of an 81-entry directory went
      **90 git forks → 10**, and 6 warm; a 6-entry directory 15 → 10.
      Fork counts are now constant in the entry count
      (`repogit.ForkCount()` + `code_tree_forks_test.go`). Read-only
      git calls on these paths also gained a 30 s deadline.
- [x] `actionsobserver`: the `octet_length` sum now runs every 5 min on its
      own cadence; the count and queue-depth gauges stay at 15 s

### Phase 4 — observability and docs (PR)

- [x] `docs/internal/observability.md` / `deploy.md` /
      `architecture.md` / `capacity.md`, plus
      `runbooks/observability.md`, `alerts.md`, `incidents.md`:
      the WireGuard + monitoring-droplet story is replaced with the
      real Alloy → Grafana Cloud pipeline. `deploy.md` and
      `architecture.md` now lead with a **single-box reference
      deployment** section (topology, per-component memory budget
      against the 3.9 GB + 4 GB swap, what the systemd ceilings
      enforce); the multi-host design is kept as a clearly marked
      aspirational section rather than deleted. Every doc now states
      plainly that there is no Alertmanager, no local Prometheus, no
      log shipping and no worker metrics endpoint, and enumerates the
      alerts that therefore do not exist.
- [x] `runbooks/incidents.md`: added a `memory-pressure` section —
      how to read the box (`journalctl -k` for OOM class, `sar -r`
      /`sar -S`/`sar -q` for the run-up, `systemctl show
      -p MemoryCurrent` + `systemd-cgtop` for per-unit attribution,
      `ps -eo rss` for the non-cgroup processes, and the `/metrics`
      gauges worth reading), plus mitigation order.
- [x] `deploy/monitoring/README.md`: states that none of the
      committed Prometheus/Alertmanager/Loki configs are deployed,
      which alerts are therefore inert, why several could not fire
      even with a Prometheus attached (job labels, no worker
      endpoint, no postgres exporter), and what adopting them would
      take. Nothing deleted.
- [x] `runbooks/alerts.md:77`: box is 2 vCPU, not 4
- [x] Backup scripts log a timestamped start/end/exit-status line to
      the cron-redirected stream and write a heartbeat file on
      success only (`/var/lib/shithub/backup-last-success`,
      `/var/lib/shithub/spaces-sync-last-success`). `set -euo
      pipefail` semantics preserved: a failed rclone still exits
      non-zero and leaves any previous heartbeat untouched. Covered
      by `scripts/test-backup-scripts.sh` (stubbed rclone/pg_dump),
      wired into `make ci` and CI alongside a `bash -n` sweep
      (`scripts/lint-shell.sh`).
- [x] The heartbeats are exported as
      `shithub_backup_last_success_seconds{job="daily"|"spaces-sync"}`
      (`internal/infra/metrics/backupobserver.go`), so backup
      freshness reaches Grafana Cloud and a managed rule can finally
      make `BackupOverdue` real. The rule in `rules.yml` named
      `shithubd_backup_last_success_seconds`, which never existed.
- [x] `postgresql.conf.j2` revised rather than deleted, for a 4 GB
      box shared with a ~200 MB app + worker: `shared_buffers=256MB`,
      `work_mem=4MB`, `effective_cache_size=1GB`,
      `maintenance_work_mem=64MB`, `max_connections=60` (web pool 10
      + worker 6 + short-lived cron/hook/ssh/admin pools + headroom),
      `shared_preload_libraries='pg_stat_statements'`,
      `pg_stat_statements.track=top`.
- [x] `docs/internal/db.md` records that the template has never been
      applied, the live-vs-template setting table, and a step-by-step
      safe-apply procedure (snapshot, `--check`, `postgres -C`
      validation, **restart** not reload, inside the 05:15–06:00
      quiet window, never a blind `make deploy`).
- [x] `check-droplet-drift.sh` tracks
      `/etc/postgresql/16/main/postgresql.conf` and reports both it
      and the `web.env` Stripe hand-edit as `KNOWN` drift with the
      reason attached.
- [x] `scripts/lint-docs-topology.sh` fails CI when `docs/internal`
      mentions WireGuard / `wg0` / `10.50.0.` outside an explicit
      `<!-- topology:aspirational-start -->` block. `docs/internal/retro/`
      is excluded — retrospectives are dated snapshots.
- [ ] Enable `pg_stat_statements` on the box — operator, see below.

## Operator to-do

Everything that needs account access or a hand on the box, with the
commands. Nothing here is done by merging a PR.

### 1. Rotate the `doctl` token — DONE 2026-09-02

Done: new token stored via `doctl auth init` (the legacy top-level
`access-token` key in doctl's config.yaml had to be blanked first,
otherwise `auth init` silently reuses the dead token).

The current token 401s, which blocks every other `doctl` item below.

```sh
# https://cloud.digitalocean.com/account/api/tokens → Generate New Token
# Scopes needed: droplet read, monitoring read+write, firewall read+write.
doctl auth init --context shithub      # paste the token when prompted
doctl auth switch --context shithub
doctl account get                      # must not 401
```

### 2. Repoint the DO uptime check at `/healthz` — DONE 2026-09-02

Done: old check deleted and `deploy/cutover/provision-do-alerts.sh`
recreated it as `159fcb49-…` targeting `https://shithub.sh/healthz`
with the down/ssl-expiry/latency alerts.

`provision-do-alerts.sh` already defaults to
`https://shithub.sh/healthz`, but `doctl` cannot change an existing
check's target in place — it needs a delete + recreate.

```sh
doctl monitoring uptime list                       # note the check id
doctl monitoring uptime delete <check-id>
deploy/cutover/provision-do-alerts.sh              # recreates check + 3 alerts
doctl monitoring uptime list --output json | jq '.[].target'
# expect "https://shithub.sh/healthz"
```

Rationale and the 429 evidence: `runbooks/alerts.md`.

### 3. Add the laptop egress IP to the runner SSH firewall — DONE 2026-09-02

Done: `74.75.126.163/32` added to `shithub-actions-runners-shared-linux`;
direct SSH to all three runners works again. Repeat when the home IP
changes.

```sh
curl -s https://ifconfig.me; echo                  # your egress IP
doctl compute firewall list                        # find the runner firewall id
doctl compute firewall add-rules <firewall-id> \
  --inbound-rules "protocol:tcp,ports:22,address:<your-ip>/32"
```

Interim workaround that needs no firewall change (the app box is
already allowed): `ssh -J root@24.199.108.81 root@<runner-ip>`.

### 4. Persist the swapfile and `vm.swappiness`

The 4 GB swapfile exists at runtime but is not in `fstab`, so it
disappears on the next reboot — which is exactly when it is most
needed. `roles/base/tasks/swap.yml` does this idempotently; use the
**same** fstab line and sysctl filename by hand so the next
`make deploy` is a no-op rather than leaving a second sysctl file
behind.

```sh
ssh root@shithub.sh '
  swapon --show
  grep -q "^/data/swapfile[[:space:]]" /etc/fstab ||
    echo "/data/swapfile none swap sw,nofail 0 0" >> /etc/fstab
  printf "vm.swappiness = 10\n" > /etc/sysctl.d/60-shithub-swap.conf
  sysctl --system
  sysctl vm.swappiness            # expect 10
  findmnt --verify --fstab 2>&1 | tail -5   # fstab parses
'
```

Do **not** verify by `swapoff`-ing: that forces every swapped page
back into a 3.9 GB box and is the one command guaranteed to cause the
outage this item exists to prevent. `swapon -a` is a no-op when the
file is already active, so the fstab line is only really exercised at
the next reboot.

Or, equivalently and preferably, just run the role:
`ANSIBLE_INVENTORY=production ANSIBLE_TAGS=base make deploy-check`
first, then `make deploy` — the swap tasks are guarded by `creates`,
the on-disk signature, `/proc/swaps` and a path-matched fstab line, so
they converge without touching the live swapfile.

### 5. Disable the duplicate AIDE timer

The Debian-packaged timer still runs alongside our cron wrapper, so
two AIDE scans overlap in the 03:00–05:00 window.

```sh
ssh root@shithub.sh '
  systemctl disable --now dailyaidecheck.timer
  systemctl is-enabled dailyaidecheck.timer      # expect: disabled
  crontab -l | grep shithub-aide-check           # our 06:00 wrapper survives
'
```

### 6. Install the defanged backup + sync scripts

The box still runs the pre-Phase-4 copies: no status lines, no
heartbeat, and the sync's `--fast-list` on the WAL bucket. Deploy
re-copies these, but do not wait for an unrelated deploy.

```sh
# from a checkout of trunk after this PR merges
scp deploy/spaces/sync-cross-region.sh  root@shithub.sh:/usr/local/bin/shithub-spaces-sync
scp deploy/postgres/backup-daily.sh     root@shithub.sh:/usr/local/bin/shithub-backup-daily
ssh root@shithub.sh '
  chmod 0755 /usr/local/bin/shithub-spaces-sync /usr/local/bin/shithub-backup-daily
  mkdir -p /var/lib/shithub /var/log/shithub
  crontab -l | grep -E "shithub-(backup-daily|spaces-sync)"   # flock + 6h cadence
'

# Verify by running each once, off-peak, and checking the traces:
ssh root@shithub.sh '
  /usr/local/bin/shithub-backup-daily >> /var/log/shithub-backup.log 2>&1
  tail -3 /var/log/shithub-backup.log
  date -u -d @"$(cat /var/lib/shithub/backup-last-success)"
'
deploy/audit/check-droplet-drift.sh    # runs locally, ssh's to the box
```

Then confirm the gauge is live:
`curl -fsS 127.0.0.1:8080/metrics | grep shithub_backup_last_success`.

### 7. Apply `postgresql.conf.j2` and enable `pg_stat_statements`

Needs a **Postgres restart**, inside the 05:15–06:00 UTC quiet
window, never via a blind `make deploy`. The full procedure with
snapshot and rollback is
[`docs/internal/db.md`](../db.md#applying-it-safely) — follow it
there rather than improvising. Ends with:

```sh
ssh root@shithub.sh 'sudo -u postgres psql -d shithub \
  -c "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"'
```

### 8. Rotate the Spaces access key

The key is plaintext in `/etc/rclone-shithub.conf`, `web.env` and
`worker.env`. Rotate if the exposure set is wider than intended.

```sh
# https://cloud.digitalocean.com/account/api/spaces → generate a new key
ssh root@shithub.sh '
  sed -i "s/^access_key_id = .*/access_key_id = <NEW>/"     /etc/rclone-shithub.conf
  sed -i "s/^secret_access_key = .*/secret_access_key = <NEW>/" /etc/rclone-shithub.conf
  # same pair in /etc/shithub/web.env and /etc/shithub/worker.env
  rclone --config /etc/rclone-shithub.conf lsd spaces-prod:   # must succeed
  systemctl restart shithubd-web shithubd-worker
'
# Only after the check above passes: revoke the old key in the portal.
```

Note `web.env` also carries a hand-edited Stripe block absent from
`web.env.j2`; a full `make deploy` re-renders the file and drops it.
`check-droplet-drift.sh` now flags this as KNOWN drift.

### 9. Enable pprof on the box

Ansible sets it in `web.env.j2`, but the deploy pipeline does not
re-render `/etc/shithub/web.env`.

```sh
ssh root@shithub.sh '
  grep -q SHITHUB_WEB__PPROF_ADDR /etc/shithub/web.env ||
    echo "SHITHUB_WEB__PPROF_ADDR=127.0.0.1:6060" >> /etc/shithub/web.env
  systemctl restart shithubd-web
  journalctl -u shithubd-web -n 20 --no-pager | grep pprof
'
# expect: pprof listener started (loopback only) addr=127.0.0.1:6060
```

### 10. Phase 0 verification, still open

```sh
ssh root@shithub.sh '
  journalctl -k --since "7 days ago" | grep -i oom     # expect empty
  sar -r | tail -20                                    # peak %memused < 75
'
# DR bucket integrity:
ssh root@shithub.sh 'rclone --config /etc/rclone-shithub.conf \
  check --size-only --one-way spaces-prod:shithub-backups spaces-dr:shithub-backups-dr'
```

### 11. Standing decision

- [ ] Resize the droplet to 8 GB if Phases 0–3 do not hold peak
      memory under 75%.

## Verification targets

- `journalctl -k | grep -i oom` empty for 7 days
- `sar -r` daily peak `%memused` < 75%
- `process_resident_memory_bytes{job="shithubd"}` < 600 MB after Phase 2
- `/metrics` series count < 1,000 after Phase 3
- DO uptime probe: zero 429 responses in Caddy access log
- `lifecycle:sweep` completes nightly without retries
