#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Keep docs/internal honest about the production topology.
#
# shithub.sh runs on ONE droplet with no VPN. The WireGuard mesh and
# the monitoring host it connects to are a design we never built, and
# for months the deploy, architecture, observability and incident docs
# described that design as if it were production — which is how an
# operator ended up told to "check wg0" on a box with no wg0.
#
# The mesh is still worth documenting as the shape we'd grow into, so
# this does not ban the words. It requires that every mention sits
# inside an explicitly marked block:
#
#   <!-- topology:aspirational-start -->
#   ... prose about the mesh, the monitoring host, etc ...
#   <!-- topology:aspirational-end -->
#
# A mention outside such a block is a claim about production, and
# fails.
#
# Excluded: docs/internal/retro/ — retrospectives are dated snapshots
# of what was believed at the time and must not be rewritten.

set -uo pipefail

cd "$(git rev-parse --show-toplevel)"

# Terms that only make sense in the multi-host design.
PATTERN='wireguard|wg0|10\.50\.0\.'

START='<!-- topology:aspirational-start -->'
END='<!-- topology:aspirational-end -->'

fail=0

while IFS= read -r f; do
  case "$f" in
    docs/internal/retro/*) continue ;;
  esac

  # Walk the file once, tracking whether we are inside a marked block,
  # and report any hit outside one. awk keeps this a single pass and
  # avoids the "grep -n then correlate line numbers" dance.
  out=$(awk -v pat="$PATTERN" -v start="$START" -v end="$END" '
    index($0, start) { inblock = 1; next }
    index($0, end)   { inblock = 0; next }
    !inblock && tolower($0) ~ pat { printf "%d: %s\n", NR, $0 }
    END {
      if (inblock) print "EOF: unclosed topology:aspirational block"
    }
  ' "$f")

  if [ -n "$out" ]; then
    echo "lint-docs-topology: $f" >&2
    printf '%s\n' "$out" | sed 's/^/  /' >&2
    fail=1
  fi
done < <(git ls-files 'docs/internal/*.md')

if [ "$fail" -ne 0 ]; then
  cat >&2 <<'MSG'

Production is a single droplet with no VPN and no monitoring host
(docs/internal/deploy.md). If the text above describes the
aspirational multi-host design, wrap it:

  <!-- topology:aspirational-start -->
  ...
  <!-- topology:aspirational-end -->

Otherwise, fix the claim.
MSG
  exit 1
fi

echo "lint-docs-topology: ok"
