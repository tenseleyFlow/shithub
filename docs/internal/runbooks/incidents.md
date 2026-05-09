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
