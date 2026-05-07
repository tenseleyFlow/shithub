#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Fail when authorization decisions leak outside internal/auth/policy.
# After S15, every "is this actor allowed to do X?" check must flow
# through policy.Can(); handler/git/cmd code must not branch on
# ownership, visibility, or collaborator state inline.
#
# The check looks for these smells in handler/git/hook code:
#   - `OwnerUserID == ` or `== row.OwnerUserID` style equality compares
#   - `Visibility == reposdb.Repo` equality compares
#   - `IsArchived` used as a control-flow predicate (not a parameter)
#
# Pure data-plumbing references (constructing sqlc.Params, returning
# the column from a query, logging) are allowed — those don't decide
# anything.
#
# Allowed locations (carved out below):
#   internal/auth/policy/...
#   internal/repos/...           (repo-create + repo orchestration)
#   internal/web/handlers/repo/  (constructs the policy actor; lookup
#                                 wrapper is policy-aware)
#   *_test.go everywhere         (tests legitimately seed state)
#
# The script exits 0 when no violations are found, 1 otherwise. Run as
# part of `make ci`.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Patterns that smell like an inline auth decision.
PATTERNS=(
  '\.OwnerUserID == '
  '\.OwnerUserID\.Int64 == '
  '== .*\.OwnerUserID'
  '\.Visibility == .*RepoVisibility'
  'if .*\.IsArchived '
)

# Files we're guarding — anywhere a request handler or hook lives.
INCLUDE=(
  ':!internal/auth/policy/*'
  ':!internal/repos/*'
  ':!internal/web/handlers/repo/*'
  ':!**/*_test.go'
)

violations=0
for pat in "${PATTERNS[@]}"; do
  matches=$(git grep -nE "$pat" -- \
    'internal/web/handlers' 'internal/git' 'cmd/shithubd' \
    "${INCLUDE[@]}" 2>/dev/null || true)
  if [[ -n "$matches" ]]; then
    echo "policy boundary violation — pattern: $pat"
    echo "$matches" | sed 's/^/  /'
    echo
    violations=1
  fi
done

if [[ "$violations" -ne 0 ]]; then
  echo "------------------------------------------------------------"
  echo "Authorization checks must flow through internal/auth/policy."
  echo "If your case is legitimate data plumbing (e.g. a sqlc.Params"
  echo "field), refactor to read the value from a struct field rather"
  echo "than compare inline."
  echo "------------------------------------------------------------"
  exit 1
fi
echo "policy boundary: clean"
