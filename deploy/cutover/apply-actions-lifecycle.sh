#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Apply the Actions object retention policy to the primary object bucket.
# Runs from the operator laptop after s3cmd is configured for the same
# DigitalOcean Spaces account used by shithub's S3 object storage.
#
# Usage:
#   SHITHUB_OBJECT_BUCKET=shithub-prod-objects \
#   ./deploy/cutover/apply-actions-lifecycle.sh

set -euo pipefail

BUCKET="${SHITHUB_OBJECT_BUCKET:?set SHITHUB_OBJECT_BUCKET to the object bucket name}"
LIFECYCLE_FILE="${LIFECYCLE_FILE:-deploy/spaces/actions-lifecycle.json}"
S3CMD="${S3CMD:-s3cmd}"

if ! command -v "$S3CMD" >/dev/null 2>&1; then
        echo "fatal: $S3CMD not on PATH; install/configure s3cmd first" >&2
        exit 2
fi
if [[ ! -f "$LIFECYCLE_FILE" ]]; then
        echo "fatal: lifecycle file not found: $LIFECYCLE_FILE" >&2
        exit 2
fi

echo "applying Actions lifecycle to s3://$BUCKET from $LIFECYCLE_FILE" >&2
"$S3CMD" setlifecycle "$LIFECYCLE_FILE" "s3://$BUCKET"

echo "current lifecycle for s3://$BUCKET:" >&2
"$S3CMD" getlifecycle "s3://$BUCKET"
