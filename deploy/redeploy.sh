#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Redeploy shithub on the app droplet from the current trunk tip.
# Invoked by .github/workflows/deploy.yml over SSH (root@shithub.sh)
# but safe to run by hand when iterating from a console session.
#
# Steps:
#   1. fetch + hard-reset to origin/trunk so we never carry local edits
#   2. build the shithubd binary into a temp path, then atomically
#      mv it onto /usr/local/bin/shithubd. systemd execve()s the new
#      file the next time the unit starts.
#   3. apply pending migrations BEFORE restarting (forward-compatible
#      schema changes only — we don't ship destructive migrations
#      gated on a binary version).
#   4. restart web + worker. systemd's TimeoutStartSec catches a wedge.
#
# Failure here surfaces as a non-zero exit on the GH Actions runner.

set -euo pipefail

REPO="${SHITHUB_REPO_DIR:-/root/src/shithub}"
BIN="${SHITHUB_BIN:-/usr/local/bin/shithubd}"
GO="${SHITHUB_GO:-/usr/local/go/bin/go}"

cd "$REPO"

git fetch --quiet origin trunk
git reset --hard origin/trunk

# Build into a sibling path then mv — atomic on the same filesystem,
# avoids a half-written binary if the build is interrupted.
TMP="$(mktemp -p "$(dirname "$BIN")" shithubd.XXXXXX)"
trap 'rm -f "$TMP"' EXIT
"$GO" build -trimpath -o "$TMP" ./cmd/shithubd
chmod 0755 "$TMP"
mv -f "$TMP" "$BIN"
trap - EXIT

# Migrations apply forward-only; no-op when already at head.
"$BIN" migrate up

systemctl restart shithubd-web
systemctl restart shithubd-worker

# Quick liveness probe so a failed restart fails the workflow instead
# of silently leaving the prior binary running.
sleep 2
systemctl is-active --quiet shithubd-web
systemctl is-active --quiet shithubd-worker

echo "redeployed $(git rev-parse --short HEAD)"
