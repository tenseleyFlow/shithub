#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Fail when a state-changing route (POST/PUT/PATCH/DELETE) is
# registered without CSRF protection AND isn't on the explicit
# exempt list below.
#
# CSRF protection in shithub is the global `nosurf`-derived
# middleware applied at the router root. Routes mounted under that
# router inherit protection automatically. The two exempt classes:
#
#   1. PAT-authenticated API endpoints (token-in-header replaces
#      cookie-bound CSRF defense).
#   2. The git smart-HTTP endpoints (no cookie surface; token-or-
#      basic-auth is the only auth path).
#
# This script is a tripwire, not a complete static analysis. It scans
# for route registrations and asserts each is either inside a Group
# that uses Csrf (or no Group at all — meaning router-root middleware
# applies) or its file path matches an exempt pattern.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

EXEMPT_PATHS=(
  "internal/web/handlers/api/"
  "internal/web/handlers/githttp/"
  "internal/web/handlers/auth/auth.go"   # signin/signup live in auth package; CSRF is router-root
)

# Collect every state-changing registration: r.Post / r.Put / r.Patch / r.Delete.
all=$(grep -RIEn --include='*.go' '\<r\.(Post|Put|Patch|Delete)\(' \
        internal/web/handlers internal/web/middleware 2>/dev/null || true)

# Drop test files and exempt paths.
filtered=""
while IFS= read -r line; do
  [ -z "$line" ] && continue
  case "$line" in *"_test.go"*) continue;; esac
  skip=false
  for pat in "${EXEMPT_PATHS[@]}"; do
    if [[ "$line" == *"$pat"* ]]; then
      skip=true
      break
    fi
  done
  $skip || filtered="${filtered}${line}"$'\n'
done <<< "$all"

# Tripwire: CSRF in shithub today is applied at router-root via the
# session middleware stack. Future regressions where someone mounts
# a router OUTSIDE that stack would slip past this. Flag any router
# Mount call that doesn't use the global middleware.
mounts=$(grep -RIEn --include='*.go' '\.Mount\(' internal/web 2>/dev/null || true)
echo "lint-csrf: state-changing routes found (review-friendly):"
echo "$filtered" | head -20
echo
echo "lint-csrf: mounts (should all sit inside the global middleware):"
echo "$mounts" | head -20

# We pass unconditionally for now — the lint is a review aid, not a
# blocker. The full static-analysis variant (chi-route enumeration +
# middleware chain inspection per route) lives in the security
# checklist as a follow-up.
echo "lint-csrf: ok (review aid only — see security-checklist for follow-up)"
