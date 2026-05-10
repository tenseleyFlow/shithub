#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Hourly health check for WAL archiving. Asserts the chain is end-
# to-end alive: Postgres is running with archive_mode=on, the
# archiver process has reported a recent success, failed_count is
# zero, and a recent segment is actually visible in Spaces.
#
# Silent on success; emits to /var/log/shithub/wal-archive.log AND
# `journalctl -t shithub-wal-archive` (warning priority) on any
# failure. Same shape as shithub-aide-check so the operator's
# muscle memory carries over.
#
# Schedule: hourly via cron (installed by ansible/roles/postgres).

set -uo pipefail

LOG=/var/log/shithub/wal-archive.log
mkdir -p "$(dirname "$LOG")"

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }

fail() {
        local msg="$*"
        {
                echo "[$(ts)] FAIL: $msg"
        } >> "$LOG"
        printf '%s\n' "$msg" | systemd-cat -t shithub-wal-archive -p warning
        exit 1
}

# 1. Postgres up + archive_mode=on.
ARCHIVE_MODE="$(sudo -u postgres psql -tAc 'SHOW archive_mode' 2>&1)"
if [[ "$ARCHIVE_MODE" != "on" ]]; then
        fail "Postgres archive_mode is '$ARCHIVE_MODE', expected 'on'"
fi

# 2. pg_stat_archiver — last_archived_time recent, failed_count==0.
# archive_timeout is 60s; allow 5x slack to absorb a missed kick.
read -r LAST_WAL LAST_TIME FAILED_COUNT LAST_FAILED_TIME < <(
        sudo -u postgres psql -tAF '|' -c "
                SELECT
                  COALESCE(last_archived_wal, ''),
                  COALESCE(EXTRACT(EPOCH FROM last_archived_time)::bigint::text, '0'),
                  failed_count,
                  COALESCE(EXTRACT(EPOCH FROM last_failed_time)::bigint::text, '0')
                FROM pg_stat_archiver;
        " | tr '|' ' '
)

NOW="$(date +%s)"
SECS_SINCE="$((NOW - LAST_TIME))"

if [[ -z "$LAST_WAL" || "$LAST_TIME" == "0" ]]; then
        fail "pg_stat_archiver has never reported a successful archive"
fi
if (( SECS_SINCE > 300 )); then
        fail "pg_stat_archiver last_archived_time is ${SECS_SINCE}s ago (>300s); archiver may be wedged"
fi
# failed_count is cumulative since the last pg_stat_reset_shared('archiver')
# — a non-zero count is fine if the failures pre-date the most recent
# success. We only flag when the most recent FAILURE is newer than the
# most recent SUCCESS (genuine ongoing breakage) AND that failure is
# recent enough to still be relevant.
if (( FAILED_COUNT > 0 && LAST_FAILED_TIME > LAST_TIME )); then
        SECS_SINCE_FAIL="$((NOW - LAST_FAILED_TIME))"
        if (( SECS_SINCE_FAIL < 600 )); then
                fail "pg_stat_archiver: most recent failure (${SECS_SINCE_FAIL}s ago) is newer than most recent success; archive_command is broken — inspect journalctl -u postgresql@16-main"
        fi
fi

# 3. The most-recent segment is visible in Spaces. We list today's
# prefix (the archive script paths under YYYY/MM/DD/) and assert
# the count is non-zero. A zero count with a recent last_archived_time
# means rclone reported success but the bucket lost the object —
# rare but worth flagging.
TODAY="$(date -u +%Y/%m/%d)"
COUNT="$(rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
        lsf "spaces-prod:shithub-wal/$TODAY/" 2>/dev/null | wc -l)"
if (( COUNT == 0 )); then
        # Edge case: it's a few minutes after UTC midnight and today's
        # prefix is genuinely empty. Look at yesterday too.
        YDAY="$(date -u -d 'yesterday' +%Y/%m/%d 2>/dev/null || \
                date -u -v-1d +%Y/%m/%d)"
        YCOUNT="$(rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
                lsf "spaces-prod:shithub-wal/$YDAY/" 2>/dev/null | wc -l)"
        if (( YCOUNT == 0 )); then
                fail "no WAL segments visible in spaces-prod:shithub-wal/$TODAY or /$YDAY despite recent archive success"
        fi
fi

# Heartbeat.
date -u +%Y-%m-%dT%H:%M:%SZ > /var/run/shithub-wal-archive.last-clean
exit 0
