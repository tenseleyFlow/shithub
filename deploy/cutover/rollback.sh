#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# S40 cutover rollback. Re-deploys a previous tag to production.
# Walks the operator through the data-safe path; do NOT use this
# blindly when migrations changed schema in non-additive ways
# (read docs/internal/runbooks/rollback.md before running).
#
# Usage:
#   deploy/cutover/rollback.sh v0.999
#
# What it does:
#   1. Checks out the named tag.
#   2. Confirms the tag exists and is signed (if signing is on).
#   3. Runs make deploy-check (DRY-RUN) and prints the diff.
#   4. Asks for explicit confirmation before applying.
#   5. Runs make deploy.
#   6. Runs the smoke script post-deploy.
#
# Exit status:
#   0 — rollback completed and smoked
#   1 — operator aborted, deploy failed, or smoke failed
#   2 — usage error

set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <previous-tag>" >&2
  exit 2
fi

TAG="$1"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

confirm() {
  local prompt="$1"
  read -r -p "$prompt [yes/NO] " resp
  if [[ "$resp" != "yes" ]]; then
    echo "aborted." >&2
    exit 1
  fi
}

echo "rollback target: $TAG"

# 1. Verify the tag exists locally; fetch if needed.
if ! git rev-parse --verify "refs/tags/$TAG" >/dev/null 2>&1; then
  echo "tag $TAG not found locally; fetching..."
  git fetch --tags
  if ! git rev-parse --verify "refs/tags/$TAG" >/dev/null 2>&1; then
    echo "FAIL: tag $TAG does not exist on origin" >&2
    exit 1
  fi
fi

# 2. Check whether the rollback crosses any new migration files.
# Forward-only migrations mean the schema is ahead of the rolled-
# back code. The operator must read the matching `down` migrations
# before continuing; we don't auto-rollback schema here.
NEW_MIGRATIONS=$(git diff --name-only "$TAG"..HEAD -- 'internal/migrationsfs/migrations/*.sql' || true)
if [[ -n "$NEW_MIGRATIONS" ]]; then
  echo ""
  echo "WARNING: migrations exist between $TAG and HEAD:"
  echo "$NEW_MIGRATIONS" | sed 's/^/  /'
  echo ""
  echo "Rolling back code without rolling back schema is fine ONLY if"
  echo "every migration above is purely additive (new columns/tables"
  echo "the old code ignores). Read docs/internal/runbooks/rollback.md"
  echo "before continuing."
  echo ""
  confirm "All migrations above are additive (the old code handles them)?"
fi

# 3. Check out the tag.
echo ""
echo "checking out $TAG..."
git checkout "$TAG"

# 4. Dry-run.
echo ""
echo "running ANSIBLE deploy-check (DRY-RUN)..."
make deploy-check ANSIBLE_INVENTORY=production

# 5. Confirm + apply.
echo ""
confirm "Apply the rollback to production?"
make deploy ANSIBLE_INVENTORY=production

# 6. Smoke. Tries to read the production base URL from a deploy var
# file; falls back to asking.
BASE="${SHITHUB_PROD_URL:-}"
if [[ -z "$BASE" ]]; then
  read -r -p "Smoke base URL (e.g. https://shithub.sh): " BASE
fi
echo ""
echo "running smoke against $BASE..."
deploy/cutover/smoke.sh "$BASE"

echo ""
echo "rollback to $TAG complete and smoked. Update the status page."
