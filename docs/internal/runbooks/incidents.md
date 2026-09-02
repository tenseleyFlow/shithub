# Incidents

Alert names in `deploy/monitoring/prometheus/rules.yml` link to the
anchors below. Keep procedures here short and actionable: what to
check first, how to mitigate, what to record. Postmortems live
elsewhere.

> **These are procedures, not pages.** There is no Alertmanager and
> no local Prometheus, so the rule file that names these anchors is
> not loaded anywhere and none of the alerts below fire on their own
> (`deploy/monitoring/README.md`). What actually notifies is the
> DigitalOcean droplet/uptime alert set in `alerts.md`. The sections
> here stay useful because they are also the checklists you run when
> a DO alert, a user report, or a metric you eyeballed in Grafana
> Cloud sends you to the box.

Everything runs on one droplet (`docs/internal/deploy.md`), so "SSH
to the affected host" always means `ssh root@shithub.sh`, and any
resource problem is a shared-resource problem: web, worker, cron,
Caddy and Postgres are competing for the same 3.9 GB.

## shithubd-down

**Symptom:** `up{job="shithubd-web"} == 0` for >2m. Site returns
502/connection refused.

1. SSH to the affected host.
2. `systemctl status shithubd-web` — if `active (running)` but the
   signal says down, the metrics path is broken, not the app:
   `systemctl status alloy` and
   `curl -fsS 127.0.0.1:8080/metrics | head -3` on the box.
3. If failed: `journalctl -u shithubd-web -n 200 --no-pager`. Look
   for migration failures (ExecStartPre), env file errors, port
   conflicts.
4. Mitigate: `systemctl restart shithubd-web`.
5. If two restarts in a row fail, roll back per `rollback.md` and
   open an incident.

## worker-down

**Symptom:** `up{job="shithubd-worker"} == 0` for >5m. Symptoms
visible to users: webhook deliveries stall, async fan-out lags.

1. `systemctl status shithubd-worker`.
2. `journalctl -u shithubd-worker -n 200`.
3. Restart. The job loop uses `FOR UPDATE SKIP LOCKED`, so a restart
   never loses or double-processes jobs.

## postgres-down

**Symptom:** `up{job="postgres"} == 0`. Site returns 500 on every
write; reads through cache may still appear to work briefly.

1. SSH to the box (Postgres is local, not a separate host).
2. `systemctl status postgresql`.
3. If startup is failing on WAL replay: do **not** delete pgdata.
   Check disk free first (`df -h /data`). If full, the most likely
   cause is the WAL archiver failing — see `archive-failing` below.
4. If the data directory is fine, page the on-call DBA. Do not
   restore from backup unless directed; a working PITR restore
   takes ~30 min and we shouldn't gamble.

## archive-failing

**Symptom:** `pg_stat_archiver.failed_count` increasing, disk filling.

1. Look at `journalctl -u postgresql -n 200` for `archive_command
   failed` lines. The last few should print the rclone error.
2. Common causes: bucket creds rotated, network partition to Spaces.
3. Confirm: `sudo -u postgres rclone --config /etc/rclone-shithub.conf
   lsd spaces-prod:`.
4. Mitigation: fix the underlying issue (rotate creds, restore
   network). Postgres will retry automatically.
5. If disk is critically full and Spaces will not be reachable in
   time: this is an emergency. Page the operator. **Do not**
   `pg_archivecleanup` manually unless directed — you can lose
   data.

## job-backlog

**Symptom:** `shithubd_job_queue_depth > 5000` for 15m.

1. Check what's in the queue:
   `psql -d shithub -c "SELECT kind, count(*) FROM jobs WHERE state='queued' GROUP BY kind ORDER BY 2 DESC;"`.
2. If one kind dominates, look for a poison job in worker logs.
3. If the spread is even, the worker isn't keeping up — scale by
   running a second worker host (the `FOR UPDATE SKIP LOCKED`
   pattern lets multiple workers coexist safely).
4. To purge a poison job: mark it `failed` (don't delete — we want
   the audit trail).

## actions-runner-heartbeat-stale

**Symptom:** `shithub_actions_runner_heartbeat_age_seconds{status!="offline"} >
60` for 5m. Actions jobs can remain queued even while the runner appears
registered.

1. Identify the runner from the alert label.
2. On the runner host: `systemctl status shithubd-runner` and
   `journalctl -u shithubd-runner -n 200 --no-pager`.
3. On the app host: `shithubd admin runner list` and confirm the
   runner labels still match queued jobs.
4. If the runner is wedged, restart `shithubd-runner`. If it cannot
   authenticate, rotate the runner token and redeploy the service env.
5. Record whether the stale heartbeat happened during a deploy, network
   partition, token rotation, or runner engine failure.

## actions-runner-idle-with-assigned-jobs

**Symptom:** `shithub_actions_runner_active_jobs{status="idle"} > 0` while the
same runner has a fresh heartbeat. The runner is reporting idle, but Postgres
still has one or more `workflow_jobs.status='running'` rows assigned to it.

1. Identify the runner from the alert label, then find its id:
   `shithubd admin runner list --output json`.
2. Inspect assigned running jobs:
   `shithubd admin runner jobs --id <runner-id>`.
3. If the runner host is reachable, check
   `journalctl -u shithubd-runner -n 200 --no-pager` first. Current runners
   send `active_job_ids` on heartbeat and should self-reconcile stale
   assignments after the next idle heartbeat.
4. If the host is dead, old, or definitely idle, run the SQL-free recovery path:
   `shithubd admin runner recover-stale-jobs --id <runner-id> --dry-run`.
   Re-run with `--confirm` only after confirming the candidate jobs are not
   actually running.
5. If some jobs are still active locally, pass each known live job with
   `--active-job-id <job-id>` so only missing assignments are cancelled.
6. After recovery, confirm the queue drains and
   `shithub_actions_runner_active_jobs{runner="<name>"}` returns to zero.

## actions-queue-depth-high

**Symptom:** `shithub_actions_queue_depth{resource="jobs"} > 100` for 10m.

1. Check runner availability:
   `shithubd admin runner list`.
2. Compare queued labels with runner labels. A workflow using an unsupported
   `runs-on` value will sit queued until a compatible runner exists.
3. Check for a stale running job assigned to a runner that is otherwise
   heartbeating idle:
   `shithubd admin runner jobs`.
   Modern runners report `active_job_ids` on heartbeat; if a runner is idle,
   the next heartbeat should cancel any running jobs missing from that set and
   increment `shithub_actions_jobs_cancelled_total{reason="runner_lost"}`.
   If the runner binary is older than that protocol, deploy the current web
   build first and then redeploy the runner role, or use
   `shithubd admin runner recover-stale-jobs --id <runner-id> --dry-run` as
   the dead-host fallback.
4. Inspect web and worker logs for trigger storms, claim errors, and DB pool
   saturation.
5. If legitimate load exceeds capacity, add runners or raise capacity on idle
   runner hosts. If one repository dominates, cancel or throttle that workload.
6. After mitigation, watch `shithub_actions_queue_depth` drain and confirm
   `shithub_actions_active` does not flatline.

## actions-run-duration-p99-regressed

**Symptom:** Actions p99 duration over 30m is >50% above the same window 24h
ago.

1. Split by event in the Actions dashboard. A single event type usually points
   to one workflow shape rather than runner infrastructure.
2. Compare `shithub_actions_active` and runner capacity. High duration with
   low active jobs suggests slow jobs; high duration with saturated active jobs
   suggests insufficient runner capacity.
3. Check runner host CPU, memory, disk, and Docker/engine logs.
4. If the regression started with a deploy, review runner API, log streaming,
   checkout, and container execution changes first.
5. Capture representative slow run IDs and their step durations before
   canceling or pruning anything.

## actions-log-scrubber-possibly-missing

**Symptom:** server-side Actions log bytes are flowing for 30m, but
`shithub_actions_log_scrub_replacements_total{location="server"}` remains zero.

This is a warning, not proof of leaked secrets. Some periods legitimately have
no secret-bearing logs.

1. Confirm secrets or variables with sensitive values exist for workloads that
   ran during the window.
2. Trigger a controlled workflow that echoes a known test secret value and
   verify the rendered logs contain `***`, not plaintext.
3. Check runner claims happened after the secret was created or rotated; mask
   snapshots are captured at claim time.
4. If the controlled workflow is not masked, stop affected runners, rotate the
   exposed secret, and open a security incident.

## memory-pressure

**Symptom:** DO `memory > 90%` alert, `load1 > 4` with low CPU,
processes stuck in `D` state, or a unit that restarted with no crash
in its own log. On a single 3.9 GB box shared by web, worker, cron,
Caddy and Postgres this is the most common incident shape — nine
kernel OOM kills in the week before 2026-09-02.

### 1. Was it the kernel?

```sh
ssh root@shithub.sh '
  journalctl -k --since "24 hours ago" | grep -iE "oom|killed process"
  journalctl --since "24 hours ago" -u shithubd-web -u shithubd-worker | grep -iE "oom|Main process exited"
'
```

`global_oom` means the whole box ran out and the kernel picked the
largest RSS. A cgroup OOM names the unit and means that unit hit its
own `MemoryMax` — different fix (raise the ceiling or fix the leak),
different blast radius (nothing else was harmed).

### 2. Read the history — `sar -r`

`sysstat` keeps 10-minute samples, so you can see the shape of the
run-up rather than the instant you happened to look.

```sh
ssh root@shithub.sh '
  sar -r | tail -40                      # today, 10-min buckets
  sar -r -f /var/log/sysstat/sa$(date -d yesterday +%d) | tail -40
  sar -r -s 03:00:00 -e 06:00:00         # the backup/AIDE window
  sar -S | tail -20                      # swap used — should stay near 0
  sar -q | tail -20                      # runqueue + load
'
```

Read `kbavail` / `%memused`, **not** `kbmemfree` — page cache counts
as used. The target from the availability campaign is a daily peak
`%memused` under 75%. Sustained `%swpused` above a few percent with
`vm.swappiness=10` means the box is genuinely over-committed, not just
paging out cold anonymous memory.

### 3. Attribute it — per-unit current usage

```sh
ssh root@shithub.sh '
  systemctl show shithubd-web    -p MemoryCurrent -p MemoryHigh -p MemoryMax
  systemctl show shithubd-worker -p MemoryCurrent -p MemoryHigh -p MemoryMax
  systemctl show shithubd-cron   -p MemoryCurrent
  systemd-cgtop -m -1 -b -n1 | head -20
'
```

`MemoryCurrent` is the cgroup total in bytes, so it includes anything
the unit forked (`git` under the worker, `pg_dump` under a cron job) —
which is the number that matters when a unit is near its ceiling.
`[not set]` means the unit has no cgroup accounting; that is a bug,
`scripts/lint-systemd-units.sh` exists to catch it.

Processes outside those cgroups — Postgres, Caddy, Alloy, rclone,
AIDE — need plain RSS:

```sh
ssh root@shithub.sh 'ps -eo rss,comm --sort=-rss | head -15'
```

Typical steady state: Postgres ~450 MB aggregate, Alloy ~320 MB,
Caddy + node_exporter + sshd ~100 MB. Transients: `rclone` 1.0–1.6 GB
during a DR sync (01/07/13/19:23 UTC), `aide` ~0.5 GB at 06:00.

### 4. Attribute it — `/metrics` gauges

From the box (or from Grafana Cloud, same series):

```sh
ssh root@shithub.sh 'curl -fsS 127.0.0.1:8080/metrics | grep -E \
  "^(process_resident_memory_bytes|go_memstats_(heap_alloc_bytes|heap_inuse_bytes|next_gc_bytes)|go_goroutines|shithub_http_in_flight|shithub_db_pool_(acquired|total))"'
```

| Gauge | Reading |
|---|---|
| `process_resident_memory_bytes` | web RSS. Target < 600 MB after the Phase 2 renderer fix; 1.3 GB was the pre-fix baseline. |
| `go_memstats_heap_alloc_bytes` | live heap. If this is flat and high while RSS climbs, the growth is not the Go heap. |
| `go_memstats_next_gc_bytes` | the GC target. With `GOMEMLIMIT=1200MiB` it should not exceed that; if it does, the env var did not reach the process. |
| `go_goroutines` | ~18 in steady state. Hundreds means a leak — take a goroutine profile. |
| `shithub_http_in_flight` | > 50 sustained means saturation is upstream of memory. |
| `shithub_db_pool_acquired` vs `_total` | equal means the pool is exhausted; workers block, and blocked work retains memory. |

These are web-process only — the worker exposes no metrics endpoint,
so `MemoryCurrent` and `ps` are the only view of it.

### 5. Mitigate

1. Kill the transient first if one is running: `pkill -f 'rclone.*shithub'`
   (the sync is idempotent; the next `flock`ed run picks up where it
   stopped), or `pkill -f 'aide --check'` (both run from root cron,
   not from a unit, so there is nothing to `systemctl stop`).
2. `systemctl restart shithubd-web` reclaims a leaked heap and costs a
   few seconds of 502.
3. Confirm swap is present and being used as a cushion, not a crutch:
   `swapon --show; sysctl vm.swappiness`. Expect a 4 GB file on
   `/data` and `vm.swappiness=10`.
4. If the run-up is a Go heap, take a profile before restarting —
   `runbooks/observability.md#taking-a-heap-profile`. A restart
   destroys the evidence.
5. Record the peak, the attribution, and whether a cron job overlapped
   the window. Recurrence at the same clock time is a scheduling
   problem, not a leak.
