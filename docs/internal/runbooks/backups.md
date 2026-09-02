# Backups

Two-layer scheme, both mandatory:

- **Continuous WAL archive** to `spaces-prod:shithub-wal` —
  `deploy/postgres/archive_command.sh` ships every WAL segment as
  Postgres rolls it. RPO is approximately one segment (~16MB or one
  `archive_timeout`).
- **Daily logical dump** to `spaces-prod:shithub-backups/daily/...`
  via `deploy/postgres/backup-daily.sh`. Keeps the most recent 7
  on the db host for fast recovery; bucket lifecycle keeps 90 days.

Cross-region mirror (`deploy/spaces/sync-cross-region.sh`) copies
both buckets into a second region for DR.

## Schedule

Everything is UTC. The spacing is deliberate — on the current
single-droplet deployment these jobs share 3.9 GB of RAM with the web
process, and the old 03:17-04:45 pile-up (pg_dump + rclone + two AIDE
scans + shithubd-cron) is what the 2026-09-02 availability sitrep
identified as an OOM trigger. Keep them apart.

| Job | When | Where |
|---|---|---|
| `shithub-spaces-sync` (cross-region mirror) | 01:23, 07:23, 13:23, 19:23 | `roles/backup` cron |
| `shithub-backup-daily` (pg_dump) | 03:17 | `roles/backup` cron |
| `shithubd-cron.timer` (sweeps + purges) | 05:15 | `deploy/systemd/shithubd-cron.timer` |
| `shithub-aide-check` (file integrity) | 06:00 | `roles/base` cron |
| `shithub-restore-drill` | Sun 04:30 | `roles/backup` cron |

Two notes on the mirror:

- It runs under `flock -n /run/lock/shithub-spaces-sync.lock`, so a
  run that overruns its slot skips the next tick instead of stacking
  a second rclone on top of itself.
- The WAL leg runs **without** `--fast-list` and at
  `--transfers 4 --checkers 8`. `--fast-list` buffers the whole
  object listing in memory; against 161k WAL objects that was ~1 GB
  of RSS. The backups bucket is small enough that it keeps
  `--fast-list`.

The packaged `dailyaidecheck.timer` is disabled by the base role —
leaving it enabled ran a second AIDE scan concurrently with ours.

## WAL archiving — first-time setup

The original provision script created `shithub-backups` and
`shithub-backups-dr` but NOT the WAL buckets, so a fresh install
ships zero WAL segments until the operator runs through this once:

1. **Create the WAL buckets** (DO Spaces dashboard — `doctl` doesn't
   manage buckets):
   - `shithub-wal` in the primary region
   - `shithub-wal-dr` in the DR region
2. **Extend the prod RW Spaces key** (Settings → API → Spaces Keys →
   Edit) to grant `readwrite` on `shithub-wal`. The `dr` key needs
   `readwrite` on `shithub-wal-dr` so `sync-cross-region.sh` can push.
3. **Confirm the rclone config on the app droplet** has both keys
   (`/etc/rclone-shithub.conf` — `spaces-prod` and
   `spaces-dr` remotes).
4. **Re-run ansible** (or drop the conf.d file by hand at
   `/etc/postgresql/16/main/conf.d/99_shithub_archive.conf`), then
   `systemctl restart postgresql@16-main` — `archive_mode` change
   needs a restart, not a reload.
5. **Verify within ~60 s**:
   ```sh
   sudo -u postgres psql -xc 'SELECT * FROM pg_stat_archiver;'
   # last_archived_wal: 000000010000000000000003 (or similar)
   # last_archived_time: <recent timestamp>
   # failed_count: 0
   rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
          lsf spaces-prod:shithub-wal/ --recursive | head
   ```
6. **If `failed_count > 0`** before any successful archive:
   `journalctl -u postgresql@16-main -n 100 | grep archive` shows
   the rclone error. Most common: bucket name typo, key grant
   missing, or the `--s3-no-check-bucket` flag is missing from the
   archive script (re-check `/usr/local/bin/shithub-pg-archive`).

## Verifying that backups are healthy

**Nothing pages on a missed backup today.** There is no Alertmanager
and no local Prometheus, so the `BackupOverdue` rule in
`deploy/monitoring/prometheus/rules.yml` never evaluates
(`deploy/monitoring/README.md`). Checking is the operator's job until
a Grafana-managed rule exists.

Each job leaves two traces:

| Job | Cron log | Heartbeat (success only) |
|---|---|---|
| `shithub-backup-daily` (03:17) | `/var/log/shithub-backup.log` | `/var/lib/shithub/backup-last-success` |
| `shithub-spaces-sync` (01/07/13/19:23) | `/var/log/shithub-spaces-sync.log` (status lines) and `/var/log/shithub/spaces-sync.log` (status + rclone detail) | `/var/lib/shithub/spaces-sync-last-success` |

Both scripts bracket every run with
`... start ...` / `... end status=ok exit=0` (or `status=FAILED
exit=N`) lines, and write the heartbeat **only** on a fully
successful run, so a stale heartbeat is never refreshed by a broken
one. Before 2026-09-02 neither log had a byte in it since 2026-05-10,
which is indistinguishable from the job never having been installed.

Quick check:

```sh
ssh root@shithub.sh '
  tail -5 /var/log/shithub-backup.log /var/log/shithub-spaces-sync.log
  for f in /var/lib/shithub/backup-last-success /var/lib/shithub/spaces-sync-last-success; do
    printf "%s: %s\n" "$f" "$(date -u -d @"$(cat "$f")" 2>/dev/null || echo MISSING)"
  done
'
```

The heartbeats are also exported as
`shithub_backup_last_success_seconds{job="daily"|"spaces-sync"}` on
`/metrics`, which Alloy pushes to Grafana Cloud — so
`time() - shithub_backup_last_success_seconds > 30h` (plus an
`absent()` clause, since the series does not exist until the first
success) is a one-rule Grafana-managed alert whenever someone wants
it.

`pg_stat_archiver.failed_count > 0` is checked hourly at :47 by
`shithub-verify-wal-archive`, which logs to
`/var/log/shithub/wal-archive.log` and journals under
`shithub-wal-archive` — also not a page. See the `archive-failing`
runbook.

To confirm the objects landed by hand:

```sh
ssh root@shithub.sh 'rclone --config /etc/rclone-shithub.conf \
     lsf spaces-prod:shithub-backups/daily/$(date -u +%Y/%m/%d)/'
```

## Quarterly restore drill

We verify the restore path **every quarter**. The calendar entry
lives in the team calendar; the procedure is in `restore.md`.

Required outputs of a successful drill:

1. `deploy/restore-drill/run.sh` exits 0.
2. The smoke queries pass.
3. The drill log has a "restore drill OK" line and is archived to
   `spaces-prod:shithub-backups/drills/<YYYY-QQ>/`.

If the drill fails: open an incident immediately. A failing drill
means our backups can't actually restore; we treat that as P0.

## Missed backup

**Symptom:** the heartbeat is older than a day, or the cron log has
a `status=FAILED` line. (`BackupOverdue` is not wired — see above.)

1. SSH to the box. There is no systemd timer; the job is a root
   crontab entry. `crontab -l | grep shithub-backup-daily` confirms
   it is installed, and
   `tail -50 /var/log/shithub-backup.log` shows the last few runs.
2. `status=FAILED exit=N` names the failing step by position: the
   line right before it in the log is the last thing that ran.
   Most likely: the script ran but `rclone copyto` failed (creds,
   network).
3. No `start` line at the expected time at all means cron did not
   fire it — check `journalctl -u cron --since yesterday`.
4. Re-run by hand: `/usr/local/bin/shithub-backup-daily`. It takes
   `sudo -u postgres` internally, so run it as root, and it is safe
   to re-run: a second dump is a new timestamped file and retention
   trims to seven.
5. If the script has been failing silently for >24h, file an
   incident — every additional day extends the RPO of an actual
   recovery.
