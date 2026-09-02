#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Daily logical backup. Run from cron (or a systemd timer) as the
# postgres user. We take a custom-format pg_dump of the shithub DB,
# stream it to Spaces, and keep one local copy in /var/backups for
# the operator to grab in a hurry. Lifecycle on the bucket prunes
# anything older than 30 days; PITR rolls forward from the WAL
# archive (see archive_command.sh).
#
# Exit non-zero on any failure. Nothing watches that exit code today
# — there is no Alertmanager and no systemd timer, this runs from root
# crontab at 03:17 with output appended to /var/log/shithub-backup.log
# — so the run also brackets itself with timestamped start/end lines
# and drops a heartbeat file on success. That log was 0 bytes from
# 2026-05-10 to 2026-09-02 precisely because a silent success and a
# silent absence look identical.

set -euo pipefail

DB="${SHITHUB_DB:-shithub}"
BUCKET="${SHITHUB_BACKUP_BUCKET:-spaces-prod:shithub-backups}"
LOCAL_DIR="${SHITHUB_BACKUP_LOCAL:-/var/backups/shithub}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
NAME="${DB}-${STAMP}.dump"

# Read by shithub_backup_last_success_seconds{job="daily"} — see
# internal/infra/metrics/backupobserver.go. Epoch seconds, one line.
HEARTBEAT="${SHITHUB_BACKUP_HEARTBEAT:-/var/lib/shithub/backup-last-success}"

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# Bracket the run. `set -e` makes most failures land here with a
# non-zero status, and the trap re-raises it so cron/systemd still see
# a failed run: the log line is an addition, never a swallow.
on_exit() {
        local rc=$?
        if [ "$rc" -eq 0 ]; then
                echo "[$(ts)] backup-daily end status=ok exit=0 dump=$NAME"
        else
                echo "[$(ts)] backup-daily end status=FAILED exit=$rc dump=$NAME"
        fi
        return "$rc"
}
trap on_exit EXIT

echo "[$(ts)] backup-daily start db=$DB bucket=$BUCKET"

mkdir -p "$LOCAL_DIR"

# pg_dump as the postgres user via local-socket peer auth.
# Cron runs this script as root; sudo handles the user switch.
sudo -u postgres pg_dump --format=custom --compress=9 --no-owner --no-privileges \
        --file="$LOCAL_DIR/$NAME" "$DB"

# Verify the dump is structurally sound before we ship it.
pg_restore --list "$LOCAL_DIR/$NAME" >/dev/null

# --s3-no-check-bucket: skip the GetBucketLocation pre-check that
# requires a permission our scoped-RW Spaces key doesn't grant.
# The actual PUT works fine on a key with bucket-level readwrite.
rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
       copyto "$LOCAL_DIR/$NAME" "$BUCKET/daily/$(date -u +%Y/%m/%d)/$NAME"

# Local retention: keep the last 7 dumps; bucket lifecycle handles
# the long tail.
ls -1t "$LOCAL_DIR"/*.dump 2>/dev/null | tail -n +8 | xargs -r rm -f

# Success only. Everything above is `set -e`-guarded, so reaching this
# line means the dump was taken, verified with pg_restore --list, and
# uploaded. Written via a temp file + mv so a reader never sees a
# half-written timestamp.
mkdir -p "$(dirname "$HEARTBEAT")"
printf '%s\n' "$(date -u +%s)" > "$HEARTBEAT.tmp"
mv "$HEARTBEAT.tmp" "$HEARTBEAT"
