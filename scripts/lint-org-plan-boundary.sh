#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Fail when paid-organization feature decisions branch directly on
# orgs.plan outside the billing/entitlement boundary. Plan storage is
# allowed in schema/sqlc plumbing, but handlers and domain packages
# should ask the entitlement layer whether a feature is available.
#
# Allowed locations:
#   internal/billing/...          — owns subscription state
#   internal/entitlements/...     — owns feature availability
#   internal/*/sqlc/...           — generated data models
#   internal/migrationsfs/...     — schema definitions
#   *_test.go everywhere          — tests may seed or assert plan values
#
# Run from `make ci`.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

PATTERN='OrgPlan(Free|Team|Enterprise)|\.Plan[[:space:]]*(==|!=)|orgs\.plan|plan[[:space:]]+org_plan'

matches=$(git grep -nE "$PATTERN" -- \
  'cmd' 'internal' \
  ':!internal/billing/*' \
  ':!internal/entitlements/*' \
  ':!internal/*/sqlc/*' \
  ':!internal/migrationsfs/*' \
  ':!**/*_test.go' \
  2>/dev/null || true)

if [[ -n "$matches" ]]; then
  echo "lint-org-plan-boundary: direct org plan feature gate outside billing/entitlements:" >&2
  echo "$matches" | sed 's/^/  /' >&2
  echo "" >&2
  echo "Fix: add or use an entitlement feature check instead of branching on orgs.plan directly." >&2
  exit 1
fi

echo "lint-org-plan-boundary: ok"
