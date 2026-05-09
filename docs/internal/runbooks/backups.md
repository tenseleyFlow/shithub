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
