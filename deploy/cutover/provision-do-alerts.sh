#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Idempotent re-provisioner for the DigitalOcean monitoring alerts
# that today aren't otherwise codified anywhere — they live in DO
# account state. Run from the operator laptop after a fresh account
# (or as a sanity-restore check) to ensure the canonical set exists.
#
# What this script provisions:
#
#   Droplet alerts (resource-utilization, scoped to shithub-app):
#     - CPU > 80% for 10m
#     - Memory > 90% for 10m
#     - Disk > 80% for 10m
#     - Load1 > 4 for 10m
#
#   Uptime check (https://shithub.sh from us_east + eu_west) plus:
#     - down_global → page on full outage (not single-region blip)
#     - ssl_expiry < 14d → catch a Caddy auto-renew failure with buffer
#     - latency p95 > 2s
#
# Idempotency: each create is gated by a list-and-match on description
# (for resource alerts) or name (for uptime check + uptime alerts).
# Re-running is a no-op once everything's in place.
#
# Prereqs:
#   - doctl installed + authenticated
#   - jq
#   - The DO account email is verified (alerts only allow verified
#     team-member emails; new emails are rejected with "email is not
#     verified"). Confirm via `doctl account get`.

set -euo pipefail

DROPLET_NAME="${DROPLET_NAME:-shithub-app}"
EMAIL="${ALERT_EMAIL:-$(doctl account get --output json | jq -r '.email')}"
UPTIME_TARGET="${UPTIME_TARGET:-https://shithub.sh}"
UPTIME_NAME="${UPTIME_NAME:-shithub.sh}"

if ! command -v doctl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
        echo "fatal: doctl + jq required" >&2
        exit 2
fi

DROPLET_ID="$(doctl compute droplet list --format ID,Name --no-header \
        | awk -v n="$DROPLET_NAME" '$2==n {print $1; exit}')"
if [[ -z "$DROPLET_ID" ]]; then
        echo "fatal: no droplet named $DROPLET_NAME" >&2
        exit 2
fi
echo "droplet: $DROPLET_NAME ($DROPLET_ID)" >&2
echo "email:   $EMAIL" >&2

# ── Droplet resource alerts ────────────────────────────────────────
ensure_resource_alert() {
        local desc="$1" type="$2" value="$3" window="$4"
        local existing
        existing="$(doctl monitoring alert list --output json \
                | jq -r --arg d "$desc" '.[] | select(.description == $d) | .uuid')"
        if [[ -n "$existing" ]]; then
                echo "  exists: $desc ($existing)" >&2
                return
        fi
        echo "  create: $desc" >&2
        doctl monitoring alert create \
                --type "$type" --compare GreaterThan --value "$value" --window "$window" \
                --entities "$DROPLET_ID" --emails "$EMAIL" \
                --description "$desc" >/dev/null
}

echo "resource alerts:" >&2
ensure_resource_alert "$DROPLET_NAME CPU > 80% for 10m"    v1/insights/droplet/cpu                          80 10m
ensure_resource_alert "$DROPLET_NAME memory > 90% for 10m" v1/insights/droplet/memory_utilization_percent   90 10m
ensure_resource_alert "$DROPLET_NAME disk > 80% for 10m"   v1/insights/droplet/disk_utilization_percent     80 10m
ensure_resource_alert "$DROPLET_NAME load1 > 4 for 10m"    v1/insights/droplet/load_1                        4 10m

# ── Uptime check ──────────────────────────────────────────────────
echo "uptime check:" >&2
CHECK_ID="$(doctl monitoring uptime list --output json \
        | jq -r --arg n "$UPTIME_NAME" '.[] | select(.name == $n) | .id')"
if [[ -z "$CHECK_ID" ]]; then
        echo "  create: $UPTIME_NAME → $UPTIME_TARGET" >&2
        CHECK_ID="$(doctl monitoring uptime create "$UPTIME_NAME" \
                --target "$UPTIME_TARGET" --type https \
                --regions us_east,eu_west --enabled true \
                --output json | jq -r '.[0].id')"
else
        echo "  exists: $UPTIME_NAME ($CHECK_ID)" >&2
fi

# ── Uptime alerts ─────────────────────────────────────────────────
ensure_uptime_alert() {
        local name="$1" type="$2" threshold="$3" comparison="$4"
        local existing
        existing="$(doctl monitoring uptime alert list "$CHECK_ID" --output json \
                | jq -r --arg n "$name" '.[] | select(.name == $n) | .id')"
        if [[ -n "$existing" ]]; then
                echo "  exists: $name ($existing)" >&2
                return
        fi
        echo "  create: $name" >&2
        doctl monitoring uptime alert create "$CHECK_ID" \
                --name "$name" --type "$type" --period 5m \
                --threshold "$threshold" --comparison "$comparison" \
                --emails "$EMAIL" >/dev/null
}

echo "uptime alerts:" >&2
ensure_uptime_alert "$UPTIME_NAME down (all regions)"          down_global  0  greater_than
ensure_uptime_alert "$UPTIME_NAME SSL cert expiring < 14 days" ssl_expiry  14  less_than
ensure_uptime_alert "$UPTIME_NAME latency p95 > 2s"            latency   2000  greater_than

echo "done." >&2
