#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Every Go source file in this repo must carry an SPDX header
# identifying the license. The convention:
#
#   // SPDX-License-Identifier: AGPL-3.0-or-later
#
# Exemptions:
# - sqlc-generated files under */sqlc/ — the sqlc tool emits a
#   different header. We don't maintain those by hand.
# - Test fixtures under testdata/.
# - Vendored code (we don't vendor today; this is forward-looking).
#
# Run: ./scripts/verify-spdx-headers.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

WANT='// SPDX-License-Identifier: AGPL-3.0-or-later'
missing=()

# Find every committed .go file outside the exemptions, then
# check the first 3 lines for the header.
while IFS= read -r f; do
  case "$f" in
    */sqlc/*) continue ;;
    */testdata/*) continue ;;
    */vendor/*) continue ;;
  esac
  if ! head -n 3 "$f" | grep -qF "$WANT"; then
    missing+=("$f")
  fi
done < <(git ls-files '*.go')

if [[ ${#missing[@]} -gt 0 ]]; then
  echo "verify-spdx-headers: the following files are missing the SPDX header:" >&2
  printf '  %s\n' "${missing[@]}" >&2
  echo "" >&2
  echo "Add this line at the top (line 1 ideally, line 3 max):" >&2
  echo "  $WANT" >&2
  exit 1
fi

# Also enforce the same on shell scripts in scripts/ and deploy/.
sh_missing=()
SH_WANT='# SPDX-License-Identifier: AGPL-3.0-or-later'
while IFS= read -r f; do
  if ! head -n 5 "$f" | grep -qF "$SH_WANT"; then
    sh_missing+=("$f")
  fi
done < <(git ls-files 'scripts/*.sh' 'deploy/**/*.sh')

if [[ ${#sh_missing[@]} -gt 0 ]]; then
  echo "verify-spdx-headers: the following shell scripts are missing the SPDX header:" >&2
  printf '  %s\n' "${sh_missing[@]}" >&2
  echo "" >&2
  echo "Add this line within the first 5 lines:" >&2
  echo "  $SH_WANT" >&2
  exit 1
fi

echo "verify-spdx-headers: ok"
