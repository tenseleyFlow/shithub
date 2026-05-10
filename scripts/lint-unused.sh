#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Fail when any Go source file carries `var _ = <symbol>` "silence
# unused import" shims. The pattern accumulated during S00–S40
# refactors (audit 2026-05-08 found 3 instances; audit 2026-05-10
# found 8 — the same shape kept growing because nothing failed CI
# on it).
#
# False positives:
# - Test files use `var _ = ...` interface assertions legitimately
#   (e.g. `var _ http.Handler = (*X)(nil)`); we exclude *_test.go.
# - Genuine type contracts of the form `var _ TypeName = (*X)(nil)`
#   are useful and explicitly allowed by the grep below — we only
#   match the `var _ = SOMETHING` (no type) form, which is the
#   shim shape.
#
# Allowlist: rare cases where a true contract assertion happens
# to take this exact shape. Add a path here and explain WHY in a
# comment if you really need an exception.

set -euo pipefail

ALLOWED_FILES=(
  # (none currently)
)

cd "$(git rev-parse --show-toplevel)"

# Match: `var _ = <ident>` at line start, possibly indented.
# Reject: `var _ Type = ...` (the legitimate interface-assertion form).
matches=$(grep -rE '^[[:space:]]*var _ = [A-Za-z]' \
  --include='*.go' \
  --exclude='*_test.go' \
  --exclude-dir='vendor' \
  --exclude-dir='.git' \
  internal/ cmd/ 2>/dev/null || true)

if [ -z "$matches" ]; then
  echo "lint-unused: ok"
  exit 0
fi

# Strip allowlisted files. ${arr[@]:-} guards against bash 3.2's
# unbound-empty-array behavior under set -u (macOS default shell).
for allowed in "${ALLOWED_FILES[@]:-}"; do
  [ -z "$allowed" ] && continue
  matches=$(echo "$matches" | grep -v "^${allowed}:" || true)
done

if [ -z "$matches" ]; then
  echo "lint-unused: ok (allowlisted hits skipped)"
  exit 0
fi

echo "lint-unused: dead-code 'silence unused import' shims found:" >&2
echo >&2
echo "$matches" >&2
echo >&2
echo "Drop the line and the import it was silencing. If the import is" >&2
echo "still needed, the shim was lying — investigate the actual usage." >&2
echo "If you need to keep one for a legitimate reason, allowlist it in" >&2
echo "scripts/lint-unused.sh with a comment explaining why." >&2
exit 1
