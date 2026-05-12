#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Atomic in-place upgrade of shithubd on the app droplet. Invoked by
# .github/workflows/deploy.yml after the runner has scp'd a freshly
# built binary to /tmp/shithubd-new. Safe to run by hand if you've
# manually placed a binary at that path.
#
# We build on the runner, not here — go.mod's toolchain version is
# usually newer than the droplet's apt-shipped Go, and shipping a
# pre-built binary keeps the droplet image lean.
#
# Steps:
#   1. fast-forward the source tree so deploy/ artifacts (this script,
#      systemd units, env templates) match the binary that just landed
#   2. atomically swap /usr/local/bin/shithubd
#   3. install app systemd unit templates and daemon-reload
#   4. apply pending migrations BEFORE restart (forward-compat only)
#   5. restart web + worker, assert is-active

set -euo pipefail

REPO="${SHITHUB_REPO_DIR:-/root/src/shithub}"
BIN="${SHITHUB_BIN:-/usr/local/bin/shithubd}"
NEW="${SHITHUB_NEW_BIN:-/tmp/shithubd-new}"

if [[ ! -x "$NEW" ]]; then
        echo "fatal: no executable at $NEW — did the runner scp it?" >&2
        exit 2
fi

cd "$REPO"
git fetch --quiet origin trunk
git reset --hard origin/trunk

# Same filesystem as $BIN so mv is atomic; chmod first because the
# scp'd file might land 0644.
chmod 0755 "$NEW"
mv -f "$NEW" "$BIN"

install -m 0644 deploy/systemd/shithubd-web.service /etc/systemd/system/shithubd-web.service
install -m 0644 deploy/systemd/shithubd-worker.service /etc/systemd/system/shithubd-worker.service
install -m 0644 deploy/systemd/shithubd-cron.service /etc/systemd/system/shithubd-cron.service
install -m 0644 deploy/systemd/shithubd-cron.timer /etc/systemd/system/shithubd-cron.timer
systemctl daemon-reload

# Migrations are usually invoked by the web unit's ExecStartPre, which
# pulls env from /etc/shithub/web.env. Replicate that here so we apply
# the schema before the restart instead of mid-startup race.
set -a
# shellcheck disable=SC1091
. /etc/shithub/web.env
set +a
"$BIN" migrate up
"$BIN" storage check

systemctl restart shithubd-web
systemctl restart shithubd-worker

sleep 2
systemctl is-active --quiet shithubd-web
systemctl is-active --quiet shithubd-worker

echo "redeployed $(git rev-parse --short HEAD)"
