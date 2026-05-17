# Incidents

Alert names in `deploy/monitoring/prometheus/rules.yml` link to the
anchors below. Keep procedures here short and actionable: what to
check first, how to mitigate, what to record. Postmortems live
elsewhere.

## shithubd-down

**Symptom:** `up{job="shithubd-web"} == 0` for >2m. Site returns
502/connection refused.

1. SSH to the affected host.
2. `systemctl status shithubd-web` — if `active (running)` but
   alert fires, the metrics scrape is broken (check wg0).
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

1. SSH to the db host.
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
3. Confirm: `sudo -u postgres rclone --config /root/.config/rclone/
   rclone.conf lsd spaces-prod:`.
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

## actions-queue-depth-high

**Symptom:** `shithub_actions_queue_depth{resource="jobs"} > 100` for 10m.

1. Check runner availability:
   `shithubd admin runner list`.
2. Compare queued labels with runner labels. A workflow using an unsupported
   `runs-on` value will sit queued until a compatible runner exists.
3. Check for a stale running job assigned to a runner that is otherwise
   heartbeating idle:
   `SELECT id, run_id, runner_id, status, started_at FROM workflow_jobs WHERE status='running' ORDER BY started_at;`.
   Modern runners report `active_job_ids` on heartbeat; if a runner is idle,
   the next heartbeat should cancel any running jobs missing from that set and
   increment `shithub_actions_jobs_cancelled_total{reason="runner_lost"}`.
   If the runner binary is older than that protocol, deploy the current web
   build first and then redeploy the runner role.
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
