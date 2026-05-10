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
# Exit non-zero on any failure so the systemd timer surfaces it
# (OnFailure= → alertmanager).

set -euo pipefail

DB="${SHITHUB_DB:-shithub}"
BUCKET="${SHITHUB_BACKUP_BUCKET:-spaces-prod:shithub-backups}"
LOCAL_DIR="${SHITHUB_BACKUP_LOCAL:-/var/backups/shithub}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
NAME="${DB}-${STAMP}.dump"

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
