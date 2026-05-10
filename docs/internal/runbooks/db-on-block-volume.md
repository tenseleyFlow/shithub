# Move Postgres data from root disk to block volume

One-time migration. After this lands, the droplet's root disk
holds only the OS + binaries; all stateful data (pgdata, repos,
tmp) lives on the attached block volume mounted at `/data`. The
goal is twofold: (1) the root disk can never fill up from
runaway DB growth and OOM the system, and (2) the volume can be
detached and reattached to a replacement droplet without losing
state if the host ever needs replacing.

## Preconditions (verify before scheduling)

- **Block volume mounted at `/data`** with enough free space for
  pgdata + headroom for several months of growth. Check:
  ```sh
  df -h /data
  ```
- **`/data/pgdata` does not contain a live cluster.** It may
  contain a stale `initdb` from when the volume was first
  provisioned — that's fine and we'll move it aside.
  ```sh
  ls -la /data/pgdata
  # Expect a PG_VERSION file dated to volume-attach time, NOT
  # to "minutes ago". If "minutes ago", STOP — something is
  # already running there.
  ```
- **Recent dump landed in Spaces** (not just locally). Worst-case
  rollback is restoring this dump:
  ```sh
  rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
         lsl spaces-prod:shithub-backups/daily/$(date -u +%Y/%m/%d)/ | tail -3
  ```
  If today's directory is empty, run a fresh dump first:
  ```sh
  /usr/local/bin/shithub-backup-daily
  ```
- **WAL archiver is healthy** — gives PITR coverage for changes
  between the last dump and migration time:
  ```sh
  sudo -u postgres psql -tAc "SELECT last_archived_time, last_failed_time FROM pg_stat_archiver;"
  # last_archived_time should be < 5 min ago, last_failed_time blank or older
  ```
- **DigitalOcean snapshot taken** via the DO dashboard
  (Droplets → shithub-prod → Snapshots → Take Snapshot). This
  is the panic-button rollback if everything else fails.
  Snapshots take a few minutes; DON'T start the migration
  until the snapshot completes.
- **Notice users** — site will be down for ~3 minutes for a
  ~115 MB pgdata. Scale the window with current `du -sh
  /var/lib/postgresql/16/main`.

## Migration

Total downtime: ~3 minutes for a 100 MB DB. Most of that is
postgres clean-shutdown + start; the rsync itself is seconds.

```sh
ssh root@shithub.sh
```

### 1. Drain writes

```sh
systemctl stop shithubd-web
systemctl stop shithubd-cron 2>/dev/null   # if running
```

Verify nothing else has DB sessions open before stopping
postgres (background workers, manual psql sessions):

```sh
sudo -u postgres psql -tAc "SELECT pid, application_name, client_addr, state FROM pg_stat_activity WHERE datname = 'shithub';"
```

If this returns rows besides your own psql, kill those processes
first.

### 2. Stop postgres

```sh
systemctl stop postgresql@16-main
systemctl status postgresql@16-main --no-pager | head -5  # should be "inactive (dead)"
```

### 3. Rename the stale pre-init aside (don't delete)

Keeping it lets us undo step 4 instantly if something looks off:

```sh
mv /data/pgdata /data/pgdata.preinit-$(date -u +%Y%m%d)
```

### 4. Copy live data to the volume

`rsync -aHX --info=progress2` preserves perms, owners, hard
links, and xattrs. `--info=progress2` shows a single overall
progress line:

```sh
rsync -aHX --info=progress2 \
  /var/lib/postgresql/16/main/ \
  /data/pgdata/
```

Verify the copy looks right:

```sh
ls -la /data/pgdata/ | head
diff <(cd /var/lib/postgresql/16/main && find . -printf '%p %s %m %u:%g\n' | sort) \
     <(cd /data/pgdata               && find . -printf '%p %s %m %u:%g\n' | sort) | head
# Empty diff = byte-identical layout.
```

### 5. Repoint the cluster

Edit the active config:

```sh
sed -i.bak "s|^data_directory = .*|data_directory = '/data/pgdata'|" \
  /etc/postgresql/16/main/postgresql.conf
grep ^data_directory /etc/postgresql/16/main/postgresql.conf
# Expect: data_directory = '/data/pgdata'
```

(`.bak` lets you `cp postgresql.conf.bak postgresql.conf` to
revert in 1 second if needed.)

### 6. Start postgres on the new path

```sh
systemctl start postgresql@16-main
sleep 2
systemctl is-active postgresql@16-main      # active
sudo -u postgres pg_isready -h /var/run/postgresql
sudo -u postgres psql -tAc "SHOW data_directory;"   # should print /data/pgdata
sudo -u postgres psql -d shithub -tAc "SELECT count(*) FROM repos;"
sudo -u postgres psql -d shithub -tAc "SELECT count(*) FROM users;"
```

If any of those fail, jump to **Rollback** below.

### 7. Bring the app back up

```sh
systemctl start shithubd-web
systemctl start shithubd-cron 2>/dev/null
systemctl is-active shithubd-web
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/healthz   # 200
```

### 8. Smoke test

From your laptop (not the droplet):

```sh
curl -fsS -o /dev/null -w '%{http_code} %{time_total}s\n' https://shithub.sh/
# Walk the site briefly: load a repo, view an issue, log in.
```

Confirm the WAL archiver picked up where it left off (next
archive timestamp should be > migration time within a couple of
minutes):

```sh
ssh root@shithub.sh 'sudo -u postgres psql -tAc "SELECT last_archived_time FROM pg_stat_archiver;"'
```

Run a fresh backup to confirm the new pgdata is durable end-to-end:

```sh
ssh root@shithub.sh /usr/local/bin/shithub-backup-daily
```

### 9. Cleanup (after a few days of healthy operation)

Don't do this until you've slept on at least one full daily
backup cycle from the new location and confirmed it landed in
Spaces. Then:

```sh
# Reclaim root-disk space.
rm -rf /var/lib/postgresql/16/main.preinit-*
# (After confirming /var/lib/postgresql/16/main is empty)
rmdir /var/lib/postgresql/16/main 2>/dev/null
rm -rf /data/pgdata.preinit-*
```

The systemd unit's `RequiresMountsFor=/var/lib/postgresql/%I`
is a static path — if you remove the empty dir, recreate it as
`mkdir -p /var/lib/postgresql/16/main && chown postgres:postgres
…` before the next reboot, otherwise `pg_ctlcluster` will refuse
to start. Easier: leave the empty dir alone.

## Rollback

### Mid-migration (steps 4–6 failed)

```sh
systemctl stop postgresql@16-main
cp /etc/postgresql/16/main/postgresql.conf.bak /etc/postgresql/16/main/postgresql.conf
mv /data/pgdata /data/pgdata.failed-$(date -u +%Y%m%d_%H%M)
mv /data/pgdata.preinit-* /data/pgdata 2>/dev/null   # restore the stale init in case something checks for it
systemctl start postgresql@16-main
systemctl start shithubd-web
```

The original `/var/lib/postgresql/16/main` was never modified, so
postgres comes back up against unchanged data. Total recovery
time: ~30 seconds.

### Worst case (data corruption observed after step 6)

```sh
# 1. Stop everything that talks to the DB.
systemctl stop shithubd-web shithubd-cron postgresql@16-main

# 2. Find the latest dump in Spaces.
rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
       lsl spaces-prod:shithub-backups/daily/ | sort | tail -1

# 3. Drop and recreate the cluster from the dump. See restore.md
#    for the full pg_restore procedure.
```

Or, faster:

### Nuclear option

Restore the droplet from the DO snapshot taken in preconditions.
Loses any user activity since the snapshot — usually that's
"the last few minutes" since the snapshot is taken right before
migration. Coordinate via status page if you do this.

## Why this layout

- **Root disk is 77 GB**; pgdata growth is unbounded. A
  runaway query log or WAL spike can fill it and freeze the
  whole droplet, including sshd.
- **Block volume is 100 GB**, separately managed, snapshotable
  in DO independently of the droplet, and detachable. If the
  droplet is unrecoverable, attaching the volume to a fresh
  droplet recovers state in minutes.
- **Repos and tmp already live on `/data`**. Moving pgdata
  finishes the layout the volume was provisioned for.
