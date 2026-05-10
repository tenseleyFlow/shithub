#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Postgres archive_command. Postgres calls this with two args:
#   $1 = absolute path to the WAL segment in pgdata
#   $2 = the segment filename
#
# Contract: exit 0 ONLY when the file is durably stored. Postgres
# refuses to recycle the segment until we report success — getting
# this wrong fills the disk. We use rclone copyto with --no-update-
# modtime so the bucket is the source of truth on retention.
#
# Wired by deploy/ansible/roles/postgres/templates/99_shithub_archive.conf.

set -euo pipefail

SRC="$1"
NAME="$2"
BUCKET="${SHITHUB_WAL_BUCKET:-spaces-prod:shithub-wal}"

# Atomic-ish: rclone copyto streams to a temp object, then renames.
# --s3-no-check-bucket: scoped Spaces keys lack GetBucketLocation; the
# actual PUT works fine on a key with bucket-level readwrite. Matches
# the same flag in backup-daily.sh + sync-cross-region.sh.
rclone --config /etc/rclone-shithub.conf \
       --s3-no-check-bucket \
       --quiet \
       copyto "$SRC" "$BUCKET/$(date +%Y/%m/%d)/$NAME"
