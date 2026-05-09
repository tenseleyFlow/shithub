#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# S40 cutover smoke test. Exercises the public-facing routes that
# matter at launch: landing page, signup/login forms render with
# a fresh CSRF token, health endpoints respond, the docs subdomain
# is reachable, the API authenticates a known PAT.
#
# Usage:
#   deploy/cutover/smoke.sh https://shithub.example
#
# Optional env (when set, the script also exercises the API):
#   SHITHUB_SMOKE_PAT     — a valid shp_ token for `user:read`
#   SHITHUB_SMOKE_DOCS    — docs subdomain URL (default: docs.<base>)
#
# Exit status:
#   0 — all green
#   1 — at least one check failed
#   2 — usage error

set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <base-url>" >&2
  exit 2
fi

BASE="$1"
DOCS="${SHITHUB_SMOKE_DOCS:-${BASE/shithub./docs.shithub.}}"
fail=0

say() { printf '\n=== %s ===\n' "$*"; }
ok()  { printf '  PASS: %s\n' "$*"; }
bad() { printf '  FAIL: %s\n' "$*"; fail=$((fail + 1)); }

# 1. Landing.
say "GET $BASE/"
body=$(curl -fsS -o - -w "\n%{http_code}\n" "$BASE/" 2>&1) || { bad "landing fetch"; body=""; }
if [[ "$body" == *"shithub"* ]]; then ok "body contains shithub"; else bad "body missing shithub"; fi

# 2. Health endpoints. /readyz proves DB + storage are reachable.
say "GET $BASE/-/health"
curl -fsS "$BASE/-/health" >/dev/null && ok "/-/health 200" || bad "/-/health"
say "GET $BASE/healthz"
curl -fsS "$BASE/healthz" >/dev/null && ok "/healthz 200" || bad "/healthz"
say "GET $BASE/readyz"
curl -fsS "$BASE/readyz" >/dev/null && ok "/readyz 200" || bad "/readyz"

# 3. Signup form renders.
say "GET $BASE/signup"
body=$(curl -fsS "$BASE/signup") || { bad "signup fetch"; body=""; }
if [[ "$body" == *"csrf_token"* ]]; then ok "CSRF token present"; else bad "no csrf_token in signup form"; fi

# 4. Login form renders.
say "GET $BASE/login"
body=$(curl -fsS "$BASE/login") || { bad "login fetch"; body=""; }
if [[ "$body" == *"username"* ]] && [[ "$body" == *"password"* ]]; then
  ok "login form fields present"
else
  bad "login form missing username/password"
fi

# 5. TLS posture. Strict-Transport-Security must be set.
say "TLS / HSTS"
hdrs=$(curl -fsS -I "$BASE/" 2>&1) || { bad "headers fetch"; hdrs=""; }
if grep -qi "strict-transport-security" <<<"$hdrs"; then
  ok "HSTS header set"
else
  bad "HSTS header missing"
fi
if grep -qi "x-content-type-options" <<<"$hdrs"; then
  ok "X-Content-Type-Options set"
else
  bad "X-Content-Type-Options missing"
fi

# 6. Docs subdomain.
say "GET $DOCS/"
curl -fsS -o /dev/null "$DOCS/" && ok "docs site 200" || bad "docs site"

# 7. API (only if a PAT is provided).
if [[ -n "${SHITHUB_SMOKE_PAT:-}" ]]; then
  say "GET $BASE/api/v1/user (with PAT)"
  body=$(curl -fsS -H "Authorization: Bearer $SHITHUB_SMOKE_PAT" "$BASE/api/v1/user") || { bad "api fetch"; body=""; }
  if [[ "$body" == *'"username"'* ]]; then
    ok "API returned a user object"
  else
    bad "API response unexpected: $body"
  fi
else
  printf '  SKIP: API check (set SHITHUB_SMOKE_PAT to run)\n'
fi

printf '\n'
if [[ "$fail" -eq 0 ]]; then
  echo "smoke: all checks passed"
  exit 0
fi
echo "smoke: $fail check(s) FAILED"
exit 1
