#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Cross-region copy from the primary Spaces bucket (NYC3) to the
# DR bucket (SFO3). Run on a schedule from the backup host (the one
# that already has rclone configured), NOT from the app server —
# we don't want a runaway sync to evict app pages from cache.
#
# rclone copy is incremental (size + mtime), so this is cheap on
# steady-state and only moves new objects.
#
# --s3-no-check-bucket: skip the GetBucketLocation pre-check that
# requires a permission our scoped-RW Spaces keys don't grant. The
# actual copy works fine on a key with bucket-level readwrite.

set -euo pipefail

PRIMARY="${SHITHUB_BACKUP_BUCKET:-spaces-prod:shithub-backups}"
DR="${SHITHUB_DR_BUCKET:-spaces-dr:shithub-backups-dr}"
WAL_PRIMARY="${SHITHUB_WAL_BUCKET:-spaces-prod:shithub-wal}"
WAL_DR="${SHITHUB_WAL_DR_BUCKET:-spaces-dr:shithub-wal-dr}"

LOG="/var/log/shithub/spaces-sync.log"
mkdir -p "$(dirname "$LOG")"

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }

{
  echo "[$(ts)] sync start"

  rclone --config /root/.config/rclone/rclone.conf --s3-no-check-bucket \
         copy --transfers 8 --checkers 16 --fast-list \
         "$PRIMARY" "$DR"

  rclone --config /root/.config/rclone/rclone.conf --s3-no-check-bucket \
         copy --transfers 8 --checkers 16 --fast-list \
         "$WAL_PRIMARY" "$WAL_DR"

  echo "[$(ts)] sync end"
} >> "$LOG" 2>&1
