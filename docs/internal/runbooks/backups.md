# Backups

Two-layer scheme, both mandatory:

- **Continuous WAL archive** to `spaces-prod:shithub-wal` —
  `deploy/postgres/archive_command.sh` ships every WAL segment as
  Postgres rolls it. RPO is approximately one segment (~16MB or one
  `archive_timeout`).
- **Daily logical dump** to `spaces-prod:shithub-backups/daily/...`
  via `deploy/postgres/backup-daily.sh`. Keeps the most recent 7
  on the db host for fast recovery; bucket lifecycle keeps 90 days.

Cross-region mirror (`deploy/spaces/sync-cross-region.sh`) runs
hourly from the backup host into a second-region DR bucket.

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
   (`/root/.config/rclone/rclone.conf` — `spaces-prod` and
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
   rclone --config /root/.config/rclone/rclone.conf --s3-no-check-bucket \
          lsf spaces-prod:shithub-wal/ --recursive | head
   ```
6. **If `failed_count > 0`** before any successful archive:
   `journalctl -u postgresql@16-main -n 100 | grep archive` shows
   the rclone error. Most common: bucket name typo, key grant
   missing, or the `--s3-no-check-bucket` flag is missing from the
   archive script (re-check `/usr/local/bin/shithub-pg-archive`).

## Verifying that backups are healthy

The monitoring stack does this for you:

- `BackupOverdue` alert fires if `time() -
  shithubd_backup_last_success_seconds > 30h`. The backup script
  pushes the timestamp to the metrics endpoint on success.
- `pg_stat_archiver.failed_count > 0` is paged via the
  `archive-failing` runbook.

If you want to confirm by hand:

```sh
ssh db
sudo -u postgres rclone --config /root/.config/rclone/rclone.conf \
     lsf spaces-prod:shithub-backups/daily/$(date -u +%Y/%m/%d)/
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

**Symptom:** `BackupOverdue` alert.

1. SSH to db host. `systemctl status shithub-backup-daily.timer`
   and `journalctl -u shithub-backup-daily.service -n 200`.
2. Most likely: the script ran but `rclone copyto` failed (creds,
   network). Re-run by hand:
   `sudo -u postgres /usr/local/bin/shithub-backup-daily`.
3. If the script has been failing silently for >24h, file an
   incident — every additional day extends the RPO of an actual
   recovery.
