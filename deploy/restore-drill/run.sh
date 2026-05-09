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

BUCKET="${SHITHUB_BACKUP_BUCKET:-spaces-prod:shithub-backups}"
WORK="$(mktemp -d -t shithub-restore-XXXXXX)"
PGDATA="$WORK/pgdata"
PGPORT="${SHITHUB_RESTORE_PGPORT:-55432}"
LOG="/var/log/shithub/restore-drill.log"
mkdir -p "$(dirname "$LOG")"

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }
say() { printf '[%s] %s\n' "$(ts)" "$*" | tee -a "$LOG"; }

cleanup() {
  if [[ -f "$PGDATA/postmaster.pid" ]]; then
    pg_ctl -D "$PGDATA" stop -m fast >/dev/null 2>&1 || true
  fi
  if [[ "$KEEP" -eq 0 ]]; then
    rm -rf "$WORK"
  else
    say "kept work dir: $WORK"
  fi
}
trap cleanup EXIT

say "restore drill start (work=$WORK port=$PGPORT)"

# 1. Resolve dump path.
if [[ -z "$DUMP" ]]; then
  LATEST="$(rclone --config /root/.config/rclone/rclone.conf \
                   lsf "$BUCKET/daily/" --recursive --files-only \
                | sort | tail -n 1)"
  if [[ -z "$LATEST" ]]; then
    say "FAIL: no dumps found in $BUCKET/daily/"
    exit 1
  fi
  DUMP="$WORK/$(basename "$LATEST")"
  say "fetching $LATEST"
  rclone --config /root/.config/rclone/rclone.conf \
         copyto "$BUCKET/daily/$LATEST" "$DUMP"
fi
say "using dump: $DUMP"

# 2. initdb + start.
initdb -D "$PGDATA" -U postgres --auth=trust --no-locale --encoding=UTF8 >/dev/null
echo "port = $PGPORT" >> "$PGDATA/postgresql.conf"
echo "unix_socket_directories = '$WORK'" >> "$PGDATA/postgresql.conf"
pg_ctl -D "$PGDATA" -l "$WORK/pg.log" -w start

# 3. Restore.
createdb -h "$WORK" -p "$PGPORT" -U postgres shithub
say "restoring..."
pg_restore --host="$WORK" --port="$PGPORT" --username=postgres \
           --dbname=shithub --no-owner --no-privileges --jobs=4 "$DUMP"

# 4. Smoke checks.
say "running smoke queries"
psql -h "$WORK" -p "$PGPORT" -U postgres -d shithub \
     -v ON_ERROR_STOP=1 -f "$(dirname "$0")/smoke-queries.sql"

say "restore drill OK"
