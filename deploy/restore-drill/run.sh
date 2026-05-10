#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Restore drill — exercises the recovery path end-to-end so that
# we know our backups actually restore. Run quarterly (the calendar
# entry is in runbooks/backups.md). The script:
#
#   1. Spins up an empty Postgres in a temp data directory.
#   2. Pulls the latest daily dump from Spaces (or an explicit
#      --dump path).
#   3. pg_restores into the temp instance.
#   4. Runs smoke-queries.sql to confirm row counts and integrity.
#   5. Tears the temp instance down.
#
# Must run as root (because rclone reads /root/.config/rclone). The
# server-side Postgres tools are invoked under sudo -u postgres
# because Postgres refuses to start as root, and the WORK directory
# is chowned to postgres so the daemon and pg_restore can read the
# dump and write to the data dir.
#
# Exits non-zero on any failure. Output is appended to
# /var/log/shithub/restore-drill.log so the on-call can review.

set -euo pipefail

DUMP=""
KEEP=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dump)  DUMP="$2"; shift 2 ;;
    --keep)  KEEP=1;    shift   ;;
    *)       echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

# Server-side tools (initdb, pg_ctl) live under the versioned bindir
# on Debian/Ubuntu and aren't on root's PATH. Pick the highest version
# present so we don't break when the cluster bumps majors.
PG_BIN="$(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V | tail -n 1)"
if [[ -z "$PG_BIN" || ! -x "$PG_BIN/initdb" ]]; then
        echo "fatal: no postgres server bindir under /usr/lib/postgresql/*/bin" >&2
        exit 2
fi

BUCKET="${SHITHUB_BACKUP_BUCKET:-spaces-prod:shithub-backups}"
WORK="$(mktemp -d -t shithub-restore-XXXXXX)"
chown postgres:postgres "$WORK"
PGDATA="$WORK/pgdata"
PGPORT="${SHITHUB_RESTORE_PGPORT:-55432}"
LOG="/var/log/shithub/restore-drill.log"
mkdir -p "$(dirname "$LOG")"

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }
say() { printf '[%s] %s\n' "$(ts)" "$*" | tee -a "$LOG"; }

cleanup() {
  if [[ -f "$PGDATA/postmaster.pid" ]]; then
    sudo -u postgres "$PG_BIN/pg_ctl" -D "$PGDATA" stop -m fast >/dev/null 2>&1 || true
  fi
  if [[ "$KEEP" -eq 0 ]]; then
    rm -rf "$WORK"
  else
    say "kept work dir: $WORK"
  fi
}
trap cleanup EXIT

say "restore drill start (work=$WORK port=$PGPORT pg=$PG_BIN)"

# 1. Resolve dump path. --s3-no-check-bucket: scoped Spaces keys lack
# GetBucketLocation; the actual GET works fine.
if [[ -z "$DUMP" ]]; then
  LATEST="$(rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
                   lsf "$BUCKET/daily/" --recursive --files-only \
                | sort | tail -n 1)"
  if [[ -z "$LATEST" ]]; then
    say "FAIL: no dumps found in $BUCKET/daily/"
    exit 1
  fi
  DUMP="$WORK/$(basename "$LATEST")"
  say "fetching $LATEST"
  rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
         copyto "$BUCKET/daily/$LATEST" "$DUMP"
fi
chown postgres:postgres "$DUMP"
say "using dump: $DUMP"

# 2. initdb + start. Run as postgres because the server refuses root.
sudo -u postgres "$PG_BIN/initdb" -D "$PGDATA" -U postgres --auth=trust --no-locale --encoding=UTF8 >/dev/null
{
  echo "port = $PGPORT"
  echo "unix_socket_directories = '$WORK'"
} | sudo -u postgres tee -a "$PGDATA/postgresql.conf" >/dev/null
sudo -u postgres "$PG_BIN/pg_ctl" -D "$PGDATA" -l "$WORK/pg.log" -w start >/dev/null

# 3. Restore.
sudo -u postgres createdb -h "$WORK" -p "$PGPORT" -U postgres shithub
say "restoring..."
sudo -u postgres pg_restore --host="$WORK" --port="$PGPORT" --username=postgres \
           --dbname=shithub --no-owner --no-privileges --jobs=4 "$DUMP" \
           >> "$LOG" 2>&1

# 4. Smoke checks. Copy the .sql into WORK so the postgres user can read it
# (the script lives under /root which is mode 0700).
cp "$(dirname "$0")/smoke-queries.sql" "$WORK/smoke.sql"
chown postgres:postgres "$WORK/smoke.sql"
say "running smoke queries"
sudo -u postgres psql -h "$WORK" -p "$PGPORT" -U postgres -d shithub \
     -v ON_ERROR_STOP=1 -f "$WORK/smoke.sql" >> "$LOG" 2>&1

say "restore drill OK"
