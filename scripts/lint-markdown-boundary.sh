#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Fail when goldmark or bluemonday is imported outside the canonical
# internal/markdown/ package. After S25, every markdown render must
# flow through markdown.Render() so the sanitizer policy and pipeline
# version stay coherent.
#
# Allowed locations:
#   internal/markdown/...     — owns Goldmark + bluemonday
#   *_test.go everywhere      — tests may exercise rendering directly
#
# Anything else triggers the alarm. The fix is to swap the import to
# `github.com/tenseleyFlow/shithub/internal/markdown` and call
# `markdown.RenderHTML` (back-compat) or `markdown.Render` (new).
#
# Exits 0 when no violations are found, 1 otherwise. Run from `make ci`.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Build a regex of forbidden imports. Matches both the bare import
# path and any aliased form.
FORBIDDEN='github\.com/(yuin/goldmark|microcosm-cc/bluemonday)'

# git grep is faster than find+grep; --null lets us safely handle
# unusual paths (we don't have any, but cheap insurance).
violations=$(git grep -lE "\"$FORBIDDEN" -- '*.go' 2>/dev/null \
    | grep -v -e '_test\.go$' \
    | grep -v -e '^internal/markdown/' \
    || true)

if [[ -n "$violations" ]]; then
    echo "lint-markdown-boundary: forbidden goldmark/bluemonday import outside internal/markdown/:" >&2
    echo "$violations" | sed 's/^/  /' >&2
    echo "" >&2
    echo "Fix: import 'github.com/tenseleyFlow/shithub/internal/markdown' and call markdown.Render or markdown.RenderHTML." >&2
    exit 1
fi

echo "lint-markdown-boundary: ok"
