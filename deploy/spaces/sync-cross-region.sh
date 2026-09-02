#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Cross-region copy from the primary Spaces bucket (NYC3) to the
# DR bucket (SFO3).
#
# Intended to run from a dedicated backup host. On the current
# single-droplet deployment it runs on the app server, which is why
# the flags below are tuned for a memory-constrained box rather than
# for throughput: the 2026-09-02 availability sitrep traced repeated
# kernel OOM kills to this script sitting at 1.0-1.6 GB RSS for ~28
# minutes of every hour alongside a 1.3 GB web process on 3.9 GB of
# RAM with no swap. Ansible now runs it every 6 h under flock, off
# the backup/AIDE window.
#
# rclone copy is incremental (size + mtime), so this is cheap on
# steady-state and only moves new objects.
#
# --fast-list buys one flat listing instead of per-directory calls,
# but buffers the ENTIRE object list in memory first. On the backups
# bucket (hundreds of objects) that is free. On the WAL bucket
# (161k objects and growing, one per 16 MB segment) it is most of a
# gigabyte, so the WAL leg lists incrementally and runs at lower
# concurrency — slower, bounded, and it no longer competes with the
# web process for the OOM killer's attention.
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

  # Small bucket: --fast-list is cheap and cuts API calls.
  rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
         copy --transfers 8 --checkers 16 --fast-list \
         "$PRIMARY" "$DR"

  # WAL bucket: no --fast-list, lower concurrency. See header.
  rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
         copy --transfers 4 --checkers 8 \
         "$WAL_PRIMARY" "$WAL_DR"

  echo "[$(ts)] sync end"
} >> "$LOG" 2>&1
