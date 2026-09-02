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

LOG="${SHITHUB_SPACES_SYNC_LOG:-/var/log/shithub/spaces-sync.log}"
mkdir -p "$(dirname "$LOG")"

# Read by shithub_backup_last_success_seconds{job="spaces-sync"} — see
# internal/infra/metrics/backupobserver.go. Epoch seconds, one line.
HEARTBEAT="${SHITHUB_SPACES_SYNC_HEARTBEAT:-/var/lib/shithub/spaces-sync-last-success}"

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# Status lines go to BOTH the script's own log and stdout, which cron
# appends to /var/log/shithub-spaces-sync.log. Before this, everything
# was swallowed into $LOG and the cron-redirected file sat at 0 bytes
# from 2026-05-10 to 2026-09-02 — indistinguishable from "the job was
# never installed". rclone's own chatter stays in $LOG only; it is far
# too noisy for the cron log.
status() { printf '[%s] %s\n' "$(ts)" "$*" | tee -a "$LOG"; }

# `set -e` routes any rclone failure here with its exit status, and
# the trap re-raises it, so a failed sync is still a non-zero exit for
# cron and for the flock wrapper.
on_exit() {
  local rc=$?
  if [ "$rc" -eq 0 ]; then
    status "spaces-sync end status=ok exit=0"
  else
    status "spaces-sync end status=FAILED exit=$rc"
  fi
  return "$rc"
}
trap on_exit EXIT

status "spaces-sync start primary=$PRIMARY dr=$DR wal=$WAL_PRIMARY wal_dr=$WAL_DR"

# Redirected per-command rather than as one `{ ... } >> "$LOG"` block:
# when `set -e` aborts inside a redirected compound command, the EXIT
# trap inherits that redirection and the FAILED line lands in $LOG
# instead of the cron log — exactly the stream we are trying to fix.

# Small bucket: --fast-list is cheap and cuts API calls.
rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
       copy --transfers 8 --checkers 16 --fast-list \
       "$PRIMARY" "$DR" >> "$LOG" 2>&1

# WAL bucket: no --fast-list, lower concurrency. See header.
rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
       copy --transfers 4 --checkers 8 \
       "$WAL_PRIMARY" "$WAL_DR" >> "$LOG" 2>&1

# Success only: both legs copied without error.
mkdir -p "$(dirname "$HEARTBEAT")"
printf '%s\n' "$(date -u +%s)" > "$HEARTBEAT.tmp"
mv "$HEARTBEAT.tmp" "$HEARTBEAT"
