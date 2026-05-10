#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# One-time provisioner for the WAL archive buckets + key grants.
# Runs from the OPERATOR'S laptop (not the droplet). Idempotent
# enough to re-run after a partial failure.
#
# Why this script exists:
#   - DO's `doctl` CLI does NOT manage Spaces buckets — bucket
#     create/delete go through the S3-compatible API directly.
#   - The original provision-do.sh created shithub-backups{,-dr}
#     and shithub-docs but skipped the WAL buckets.
#   - The existing scoped Spaces keys can't create buckets; only
#     a FullAccess key can. So this script:
#       1. Mints a temporary FullAccess Spaces key via doctl.
#       2. SSHes to the app droplet (which already has rclone) to
#          PUT-create both buckets using that temp key. Creds are
#          passed via env vars on the SSH command line, never
#          written to disk.
#       3. Deletes the temp FullAccess key (security hygiene —
#          minimize the lifetime of a key that can do anything).
#       4. Extends the existing scoped RW key's grants to include
#          readwrite on shithub-wal{,-dr}.
#       5. Verifies the droplet can write to both buckets through
#          its production rclone config.
#
# Prereqs (one-time on the operator laptop):
#   - doctl installed + authenticated (`doctl auth init`)
#   - ssh access to root@$DEPLOY_HOST (the same key that the GH
#     Actions deploy uses works fine)
#
# Usage:
#   DEPLOY_HOST=shithub.sh \
#   PRIMARY_REGION=sfo3 DR_REGION=ams3 \
#   PROD_RW_KEY_NAME=shithub-prod-app-rw \
#   ./deploy/cutover/provision-wal-buckets.sh

set -euo pipefail

DEPLOY_HOST="${DEPLOY_HOST:?set DEPLOY_HOST (e.g. shithub.sh)}"
PRIMARY_REGION="${PRIMARY_REGION:-sfo3}"
DR_REGION="${DR_REGION:-ams3}"
PROD_RW_KEY_NAME="${PROD_RW_KEY_NAME:-shithub-prod-app-rw}"

WAL_BUCKET="${WAL_BUCKET:-shithub-wal}"
WAL_DR_BUCKET="${WAL_DR_BUCKET:-shithub-wal-dr}"

if ! command -v doctl >/dev/null 2>&1; then
        echo "fatal: doctl not on PATH; brew install doctl  /  apt-get install doctl" >&2
        exit 2
fi
if ! doctl account get >/dev/null 2>&1; then
        echo "fatal: doctl not authenticated; run 'doctl auth init'" >&2
        exit 2
fi

# ── Step 1: mint a temporary FullAccess Spaces key.
TMP_KEY_NAME="shithub-wal-bootstrap-$(date -u +%Y%m%dT%H%M%SZ)"
echo "minting temp FullAccess key: $TMP_KEY_NAME" >&2
TMP_KEY_JSON="$(doctl spaces keys create "$TMP_KEY_NAME" \
        --grants 'bucket=;permission=fullaccess' \
        --output json)"
TMP_ACCESS_KEY="$(printf '%s' "$TMP_KEY_JSON" | jq -r '.[0].access_key')"
TMP_SECRET_KEY="$(printf '%s' "$TMP_KEY_JSON" | jq -r '.[0].secret_key')"
if [[ -z "$TMP_ACCESS_KEY" || "$TMP_ACCESS_KEY" == "null" ]]; then
        echo "fatal: failed to extract access_key from doctl response: $TMP_KEY_JSON" >&2
        exit 2
fi

# Always clean up the temp key, even on partial failure.
cleanup_temp_key() {
        local rc=$?
        echo "deleting temp FullAccess key $TMP_ACCESS_KEY..." >&2
        doctl spaces keys delete "$TMP_ACCESS_KEY" --force >&2 || \
                echo "WARN: temp key delete failed; revoke manually via dashboard" >&2
        exit "$rc"
}
trap cleanup_temp_key EXIT

# ── Step 2: PUT-create both buckets via rclone on the droplet.
# We use rclone's inline-config syntax (`:s3,...`) so no rclone.conf
# edit is needed and the temp creds never land on disk. The buckets
# are EMPTY after creation; pg_archive starts shipping segments on
# the next archive_timeout once Postgres is reconfigured.
ssh_create_bucket() {
        local bucket="$1" region="$2"
        echo "creating $bucket in $region..." >&2
        ssh -o BatchMode=yes "root@$DEPLOY_HOST" \
                "AKEY='$TMP_ACCESS_KEY' SKEY='$TMP_SECRET_KEY' \
                 rclone mkdir \
                 ':s3,provider=DigitalOcean,access_key_id='\$AKEY',secret_access_key='\$SKEY',endpoint=$region.digitaloceanspaces.com:$bucket' 2>&1"
}
ssh_create_bucket "$WAL_BUCKET"    "$PRIMARY_REGION"
ssh_create_bucket "$WAL_DR_BUCKET" "$DR_REGION"

# ── Step 3: temp key cleanup happens via the EXIT trap above.

# ── Step 4: extend the scoped prod RW key's grants.
echo "looking up existing prod RW key id..." >&2
PROD_KEY_LINE="$(doctl spaces keys list --output json \
        | jq -r --arg n "$PROD_RW_KEY_NAME" '.[] | select(.name == $n)')"
PROD_KEY_ID="$(printf '%s' "$PROD_KEY_LINE" | jq -r '.access_key')"
PROD_KEY_GRANTS="$(printf '%s' "$PROD_KEY_LINE" | jq -r '.grants')"
if [[ -z "$PROD_KEY_ID" || "$PROD_KEY_ID" == "null" ]]; then
        echo "fatal: no Spaces key named $PROD_RW_KEY_NAME" >&2
        exit 2
fi
echo "found $PROD_RW_KEY_NAME ($PROD_KEY_ID); current grants: $PROD_KEY_GRANTS" >&2

# Build the new grants string from the existing JSON, adding wal +
# wal-dr at readwrite if absent. doctl wants the comma-separated
# `bucket=NAME;permission=PERM,...` shape on update.
NEW_GRANTS="$(printf '%s' "$PROD_KEY_LINE" \
        | jq -r '
                .grants
                | (if (any(.bucket == "'"$WAL_BUCKET"'")) then . else . + [{bucket: "'"$WAL_BUCKET"'", permission: "readwrite"}] end)
                | (if (any(.bucket == "'"$WAL_DR_BUCKET"'")) then . else . + [{bucket: "'"$WAL_DR_BUCKET"'", permission: "readwrite"}] end)
                | map("bucket=\(.bucket);permission=\(.permission)")
                | join(",")
        ')"
echo "new grants: $NEW_GRANTS" >&2
doctl spaces keys update "$PROD_KEY_ID" \
        --name "$PROD_RW_KEY_NAME" \
        --grants "$NEW_GRANTS" >&2

# ── Step 5: verify the droplet's existing rclone can now write to
# both buckets using its on-disk config (which references the just-
# updated scoped key). A failure here means either the key cache
# hasn't propagated (wait 30s, re-run) or the scoped key isn't the
# one in /etc/rclone-shithub.conf (check by hand).
echo "verifying droplet → both WAL buckets..." >&2
ssh -o BatchMode=yes "root@$DEPLOY_HOST" "
        set -e
        echo wal-write-probe-\$(date -u +%Y%m%dT%H%M%SZ) \
                | rclone --config /etc/rclone-shithub.conf \
                        --s3-no-check-bucket \
                        rcat spaces-prod:$WAL_BUCKET/.write-probe
        echo wal-write-probe-\$(date -u +%Y%m%dT%H%M%SZ) \
                | rclone --config /etc/rclone-shithub.conf \
                        --s3-no-check-bucket \
                        rcat spaces-dr:$WAL_DR_BUCKET/.write-probe
        rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket \
                delete spaces-prod:$WAL_BUCKET/.write-probe spaces-dr:$WAL_DR_BUCKET/.write-probe
        echo OK
"

cat <<DONE >&2

==============================================================
WAL buckets provisioned.

  $WAL_BUCKET     ($PRIMARY_REGION)
  $WAL_DR_BUCKET  ($DR_REGION)

The prod RW Spaces key now has readwrite on both. Next step:

  ssh root@$DEPLOY_HOST 'systemctl restart postgresql@16-main'

(only needed if the conf.d drop-in at
/etc/postgresql/16/main/conf.d/99_shithub_archive.conf is
already in place — see the WAL archive PR for that change).

Verify within ~60s:
  ssh root@$DEPLOY_HOST 'sudo -u postgres psql -xc "SELECT * FROM pg_stat_archiver"'
  ssh root@$DEPLOY_HOST 'rclone --config /etc/rclone-shithub.conf --s3-no-check-bucket lsf spaces-prod:$WAL_BUCKET/ --recursive | head'
==============================================================
DONE
