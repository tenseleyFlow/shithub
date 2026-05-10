# Backup & restore

Two layers, both mandatory:

- **Continuous WAL archive** — `deploy/postgres/archive_command.sh`
  ships every WAL segment to `spaces-prod:shithub-wal` in real
  time. Recovery point objective (RPO) ≈ one WAL segment (~16 MB
  or one `archive_timeout`).
- **Daily logical dump** — `deploy/postgres/backup-daily.sh`
  takes a `pg_dump --format=custom` once per day and ships it to
  `spaces-prod:shithub-backups/daily/YYYY/MM/DD/`. Keeps the most
  recent 7 on the db host.

Cross-region copy (`deploy/spaces/sync-cross-region.sh`) mirrors
both buckets to a second region for DR. Lifecycle in
`deploy/spaces/lifecycle.json` prunes WAL after 30 days and dumps
after 90.

## Verifying that backups are healthy

The monitoring stack does this for you:

- `BackupOverdue` alert fires if no successful backup in 30h.
- `pg_stat_archiver.failed_count > 0` is paged via the
  archive-failing alert.

By hand:

```sh
ssh db
sudo -u postgres rclone --config /etc/rclone-shithub.conf \
     lsf spaces-prod:shithub-backups/daily/$(date -u +%Y/%m/%d)/
```

## Restore drills

Run **quarterly**. The restore drill restores the latest dump
into a temp Postgres instance, runs smoke queries, tears down.
Production is untouched.

```sh
ssh backup-host
sudo /usr/local/bin/shithub-restore-drill
# uses the latest dump in spaces-prod:shithub-backups/daily/
```

To drill an older dump:

```sh
sudo /usr/local/bin/shithub-restore-drill --dump /path/to/file.dump
```

`--keep` preserves the temp pgdata for inspection. Default is
clean-up.

A failed drill is a P0: it means our backups can't actually
restore. File an incident immediately.

## Real restore — full DB lost

Use this when:

- The DB host is destroyed.
- Postgres can't start and there's no working WAL.
- 24 hours of data loss is acceptable.

1. Spin up a new db host
   (`make deploy ANSIBLE_INVENTORY=production ANSIBLE_LIMIT=db-new
   ANSIBLE_TAGS=db`).
2. Pull the most recent daily dump:
   ```sh
   rclone copyto \
     spaces-prod:shithub-backups/daily/$(date -u +%Y/%m/%d)/<latest>.dump \
     /tmp/restore.dump
   ```
3. Restore:
   ```sh
   pg_restore --dbname=shithub --jobs=4 --no-owner --no-privileges /tmp/restore.dump
   ```
4. Confirm schema: `shithubd migrate status`.
5. Bring the app up against the new DB (update `web.env` and
   `worker.env`).
6. **Notify users** — there will be a visible activity gap.

## Real restore — point-in-time

Use this when data is intact in WAL but a destructive change
happened at a known time (`DROP TABLE`, mass UPDATE, runaway
worker).

This is harder. The procedure:

1. Stop the app (`systemctl stop shithubd-web shithubd-worker
   shithubd-cron.timer`). Do not let writes continue.
2. Restore the most recent base backup from before the incident
   into a fresh data directory.
3. Configure recovery (`recovery.signal` + GUCs in PG16) pointing
   at the WAL archive in Spaces with
   `recovery_target_time = '<UTC timestamp just before incident>'`.
4. Start Postgres in recovery mode; let it replay WAL.
5. When recovery completes, promote.
6. Restart the app.

If you have not done a PITR before, **do not** free-style this.
Mistakes here destroy the data you were trying to save. Find
someone who has done it and pair with them.

## After any real restore

- Run the restore-drill smoke queries against the restored DB.
- Reconcile the audit log against any new IDs that don't exist.
- Force-rotate webhook secrets (`shithubd webhook rotate-all`) —
  if the dump was compromised, the secrets in it may be too.
- Force-rotate session epochs for any user whose session crossed
  the recovery window.
