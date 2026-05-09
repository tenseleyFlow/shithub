#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Build the public docs site and sync it to the Spaces bucket
# that Caddy serves docs.shithub.example from.
#
# Run from CI on every push to main, or from an operator's
# workstation as a one-off. Idempotent: rclone sync only touches
# changed files.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

BUCKET="${SHITHUB_DOCS_BUCKET:-spaces-docs:shithub-docs}"

if ! command -v mdbook >/dev/null 2>&1; then
  echo "fatal: mdbook not on PATH; install from https://rust-lang.github.io/mdBook/" >&2
  exit 2
fi

# Build into the configured build-dir from book.toml (build/docs/).
echo "building docs..."
cd docs/public && mdbook build && cd "$ROOT"

if [[ ! -d build/docs ]]; then
  echo "fatal: mdbook build did not produce build/docs/" >&2
  exit 2
fi

echo "syncing to $BUCKET..."
rclone --config /root/.config/rclone/rclone.conf \
       sync --transfers 8 --checkers 16 \
       build/docs "$BUCKET"

echo "docs sync complete"
