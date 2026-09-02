#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Syntax check every shell script in the tree. Most of deploy/ is
# shell that only ever executes on the droplet, from cron, unattended
# — a parse error there is discovered by a missed backup, not by a
# failing build. `bash -n` is cheap and catches exactly that class.
#
# shellcheck is not a CI dependency (it isn't installed on the
# runner); when it happens to be present locally its findings are
# reported as advisory and never fail the run.

set -uo pipefail

cd "$(git rev-parse --show-toplevel)"

fail=0
checked=0

while IFS= read -r f; do
  checked=$((checked + 1))
  if ! out=$(bash -n "$f" 2>&1); then
    echo "lint-shell: $f: $out" >&2
    fail=1
  fi
done < <(git ls-files '*.sh')

if [ "$fail" -ne 0 ]; then
  echo "lint-shell: syntax errors above" >&2
  exit 1
fi

if command -v shellcheck >/dev/null 2>&1; then
  # Advisory only: the tree has never been shellcheck-clean and
  # gating on it here would block unrelated work.
  git ls-files '*.sh' | xargs shellcheck --severity=error --format=gcc 2>/dev/null || true
fi

echo "lint-shell: ok ($checked scripts)"
