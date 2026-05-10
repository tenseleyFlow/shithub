#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Goose keys migrations by numeric version, not the rest of the filename.
# Duplicate NNNN prefixes panic at runtime before any migration runs, so
# catch them in CI before a deploy has to discover the conflict.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

migrations=$(find internal/migrationsfs/migrations -maxdepth 1 -type f -name '[0-9][0-9][0-9][0-9]_*.sql' | sort)
versions=$(printf '%s\n' "$migrations" | sed -E 's#^.*/([0-9][0-9][0-9][0-9])_.*$#\1#')
duplicates=$(printf '%s\n' "$versions" | sort | uniq -d)

if [ -n "$duplicates" ]; then
  echo "lint-migration-versions: duplicate migration versions found:" >&2
  echo >&2
  while IFS= read -r version; do
    [ -z "$version" ] && continue
    echo "  version $version:" >&2
    printf '%s\n' "$migrations" | grep "/${version}_" | sed 's/^/    /' >&2
  done <<< "$duplicates"
  echo >&2
  echo "Goose requires each numeric NNNN prefix to be globally unique." >&2
  exit 1
fi

echo "lint-migration-versions: ok"
