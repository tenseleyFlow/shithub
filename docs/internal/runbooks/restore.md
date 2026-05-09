# Restore

Two restore modes:

- **Drill restore** — done quarterly into a temp PG instance to
  verify the backup chain works. Never touches the production DB.
  Procedure: the drill script. See "Drill" below.
- **Real restore** — used after a data-loss incident. This is the
  procedure in "Real restore" below. Read it once before you ever
  need it.

## Drill

```sh
ssh backup-host
sudo /usr/local/bin/shithub-restore-drill
# uses the latest dump in spaces-prod:shithub-backups/daily/
```

To drill an older dump:

```sh
sudo /usr/local/bin/shithub-restore-drill --dump /path/to/file.dump
```

`--keep` preserves the temp pgdata for inspection. The default is
clean-up.

The script writes to `/var/log/shithub/restore-drill.log`. After a
successful drill, archive that log:

```sh
rclone copyto /var/log/shithub/restore-drill.log \
       spaces-prod:shithub-backups/drills/$(date -u +%Y-Q)/restore-$(date -u +%Y%m%dT%H%M%SZ).log
```

## Real restore

Two scenarios. **Read the right one** — they have different recovery
points and different blast radius.

### A. Restore-from-daily (full DB lost, OK with up-to-24h data loss)

Use this when:

- The DB host is destroyed.
- Postgres can't start and we have no working WAL.
- We accept losing up to a day of writes (RPO = 24h).

Procedure:

1. Spin up a new db host (Ansible: `ANSIBLE_LIMIT=db-new make deploy
   --tags db`).
2. Pull the most recent daily dump:
   ```sh
   rclone copyto spaces-prod:shithub-backups/daily/$(date -u +%Y/%m/%d)/<latest>.dump /tmp/restore.dump
   ```
3. `pg_restore --dbname=shithub --jobs=4 --no-owner --no-privileges /tmp/restore.dump`
4. Run `shithubd migrate status` to confirm schema version.
5. Bring the app up against the new DB (update `web.env` and
   `worker.env`). Verify with smoke queries from the drill suite.
6. **Notify users** — there will be a visible gap in their
   activity. Don't make them figure it out.

### B. PITR (recover to a specific moment, e.g., before a bad migration)

Use this when:

- The data is intact in WAL but a destructive change happened at a
  known time (`DROP TABLE`, mass `UPDATE`, runaway worker).
- We need to roll the DB back to immediately before that moment.

This is harder; the procedure is:

1. Stop the app (`systemctl stop shithubd-web shithubd-worker
   shithubd-cron.timer`). Do not let writes continue.
2. Restore the most recent base backup from before the incident
   into a fresh data directory.
3. Configure `recovery.conf` (or `recovery.signal` + GUCs in PG16)
   pointing at the WAL archive in Spaces with
   `recovery_target_time = '<UTC timestamp just before incident>'`.
4. Start Postgres in recovery mode, let it replay WAL.
5. When recovery completes, promote.
6. Restart the app.

If you have not done a PITR before: page someone who has, do not
free-style this. Mistakes here lose the data you were trying to
save.

## After any real restore

- Run the restore-drill smoke queries against the restored DB.
- Reconcile the audit log: any rows referencing IDs that no longer
  exist need a note in the incident postmortem.
- Force-rotate webhook secrets (`shithubd webhook rotate-all`) —
  if an attacker triggered the recovery, the secrets in the dump
  may be compromised.
- Force-rotate session-epochs for any user whose session crossed
  the recovery window.
