#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Verify that every /api/v1 route registered in
# internal/web/handlers/api/ appears in docs/public/api/.
#
# We don't enforce on a per-page basis — the route just needs to
# show up *somewhere* under docs/public/api/. The page-organization
# is the human's call; the script catches the most common drift
# bug (a new endpoint shipped, no doc updated).
#
# Excluded by design: planned-only endpoints in the docs that
# the source doesn't yet implement (routes mentioned in tables
# under "## Planned routes" headings). The verifier is route →
# doc, not doc → route.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# Collect every /api/v1 route literal from chi route registrations.
# Pattern: r.<Method>("/api/v1/<path>", ...)
ROUTES_FILE="$(mktemp)"
trap 'rm -f "$ROUTES_FILE"' EXIT

grep -rhoE 'r\.(Get|Post|Put|Patch|Delete|Head|Options)\("/api/v1[^"]*"' \
     internal/web/handlers/api 2>/dev/null \
  | sed -E 's/^r\.[A-Z][a-z]+\("([^"]+)".*$/\1/' \
  | sort -u > "$ROUTES_FILE"

route_count=$(wc -l < "$ROUTES_FILE" | tr -d ' ')
if [[ "$route_count" -eq 0 ]]; then
  echo "verify-api-docs: no API routes found — is the package layout right?" >&2
  exit 2
fi

# Concatenate every doc page into one searchable blob.
DOC_BLOB="$(cat docs/public/api/*.md 2>/dev/null || true)"
if [[ -z "$DOC_BLOB" ]]; then
  echo "verify-api-docs: docs/public/api/ is empty" >&2
  exit 2
fi

missing_count=0
while IFS= read -r route; do
  if ! grep -qF "$route" <<<"$DOC_BLOB"; then
    if [[ "$missing_count" -eq 0 ]]; then
      echo "verify-api-docs: the following routes are not documented under docs/public/api/:" >&2
    fi
    echo "  $route" >&2
    missing_count=$((missing_count + 1))
  fi
done < "$ROUTES_FILE"

if [[ "$missing_count" -gt 0 ]]; then
  echo "" >&2
  echo "Add a section that mentions the route literal verbatim." >&2
  exit 1
fi

echo "verify-api-docs: all ${route_count} routes documented"
