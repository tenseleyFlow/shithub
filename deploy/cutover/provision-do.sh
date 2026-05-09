#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# UI-free provision script for the four-droplet + three-Spaces +
# one-volume topology described in deploy/cutover/SETUP-GUIDE.md
# Phase B. Uses `doctl`, DO's CLI — its commands are stable across
# UI redesigns and the source of truth for what Phase B should
# produce.
#
# Why prefer this over the dashboard:
# - Reproducible: same flags → same resources, every time.
# - No UI drift: `doctl` is versioned and changelogged.
# - Cheaper to recover from a botched run: destroy + re-run.
# - The dashboard remains a fine choice; this is the alternative.
#
# Prereqs (do these once, then re-run is idempotent-friendly):
#   - `doctl` installed: brew install doctl  /  apt-get install doctl
#   - Authenticated: doctl auth init   (paste a Personal Access Token
#     from DO → API → Tokens with read+write scope)
#   - Your laptop's ~/.ssh/id_ed25519.pub uploaded to your account
#     (doctl compute ssh-key import or via the dashboard).
#
# Usage:
#   PRIMARY_REGION=sfo3 DR_REGION=ams3 \
#   PROJECT_NAME=shithub-prod \
#   SSH_KEY_NAME=macbook-pro \
#   ./deploy/cutover/provision-do.sh
#
# What it creates:
#   - 1 project (if not present)
#   - 4 droplets (app, db, backup, monitoring) — all in PRIMARY_REGION
#   - 1 100 GB volume attached to shithub-app
#   - 3 Spaces buckets (shithub-backups in PRIMARY_REGION,
#       shithub-backups-dr in DR_REGION, shithub-docs in PRIMARY_REGION)
#
# What it does NOT do:
#   - Generate Spaces access keys (do that via API → Spaces Keys in
#     the UI; doctl can't surface the secret either).
#   - Configure CDN custom domain on shithub-docs.
#   - Set DNS records on Namecheap.
#
# Safe to re-run: it skips resources that already exist by name.

set -euo pipefail

PRIMARY_REGION="${PRIMARY_REGION:-sfo3}"
DR_REGION="${DR_REGION:-ams3}"
PROJECT_NAME="${PROJECT_NAME:-shithub-prod}"
SSH_KEY_NAME="${SSH_KEY_NAME:?set SSH_KEY_NAME to the name of the SSH key in your DO account}"

if ! command -v doctl >/dev/null 2>&1; then
  echo "fatal: doctl not on PATH; install from https://docs.digitalocean.com/reference/doctl/" >&2
  exit 2
fi

if ! doctl account get >/dev/null 2>&1; then
  echo "fatal: doctl not authenticated; run 'doctl auth init'" >&2
  exit 2
fi

# Resolve the SSH key id by name.
SSH_KEY_ID="$(doctl compute ssh-key list --no-header --format ID,Name | awk -v n="$SSH_KEY_NAME" '$2==n {print $1; exit}')"
if [[ -z "$SSH_KEY_ID" ]]; then
  echo "fatal: no SSH key named $SSH_KEY_NAME in your DO account" >&2
  echo "(list with: doctl compute ssh-key list)" >&2
  exit 2
fi
echo "using SSH key: $SSH_KEY_NAME (id $SSH_KEY_ID)"

# --- 1. Project (idempotent: skip if name already exists) ---
PROJECT_ID="$(doctl projects list --no-header --format ID,Name | awk -v n="$PROJECT_NAME" '$2==n {print $1; exit}')"
if [[ -z "$PROJECT_ID" ]]; then
  echo "creating project $PROJECT_NAME..."
  PROJECT_ID="$(doctl projects create \
    --name "$PROJECT_NAME" \
    --purpose "Service or API" \
    --environment Production \
    --description "shithub.sh production environment" \
    --no-header --format ID)"
fi
echo "project: $PROJECT_NAME (id $PROJECT_ID)"

# --- 2. Droplets ---
create_or_skip_droplet() {
  local name="$1" size="$2" tag="$3"
  local existing
  existing="$(doctl compute droplet list --no-header --format ID,Name | awk -v n="$name" '$2==n {print $1; exit}')"
  if [[ -n "$existing" ]]; then
    echo "droplet $name already exists (id $existing); skipping"
    echo "$existing"
    return
  fi
  echo "creating droplet $name (size $size)..."
  local id
  id="$(doctl compute droplet create "$name" \
    --image ubuntu-24-04-x64 \
    --region "$PRIMARY_REGION" \
    --size "$size" \
    --ssh-keys "$SSH_KEY_ID" \
    --enable-monitoring \
    --tag-names "shithub,$tag" \
    --wait \
    --no-header --format ID)"
  echo "$id"
}

APP_ID="$(create_or_skip_droplet shithub-app        s-2vcpu-4gb shithub-app)"
DB_ID="$(create_or_skip_droplet shithub-db          s-2vcpu-4gb shithub-db)"
BAK_ID="$(create_or_skip_droplet shithub-backup     s-1vcpu-2gb shithub-backup)"
MON_ID="$(create_or_skip_droplet shithub-monitoring s-2vcpu-4gb shithub-monitoring)"

# --- 3. Block volume + attach to shithub-app ---
VOL_NAME="shithub-data"
VOL_ID="$(doctl compute volume list --no-header --format ID,Name | awk -v n="$VOL_NAME" '$2==n {print $1; exit}')"
if [[ -z "$VOL_ID" ]]; then
  echo "creating 100 GB volume $VOL_NAME..."
  VOL_ID="$(doctl compute volume create "$VOL_NAME" \
    --region "$PRIMARY_REGION" \
    --size 100GiB \
    --fs-type ext4 \
    --no-header --format ID)"
  echo "attaching $VOL_NAME to shithub-app..."
  doctl compute volume-action attach "$VOL_ID" "$APP_ID" --wait
else
  echo "volume $VOL_NAME already exists (id $VOL_ID); skipping create"
fi

# --- 4. Spaces buckets ---
# doctl's spaces support exists but is limited; fall through to the s3-compatible API
# via the standard awscli isn't worth the extra dep here. We use doctl's native
# Spaces commands where they exist, and fall back to instructions for the rest.
create_or_skip_space() {
  local name="$1" region="$2"
  if doctl spaces buckets list --no-header --format Name | grep -qx "$name" 2>/dev/null; then
    echo "Spaces bucket $name already exists; skipping"
    return
  fi
  echo "creating Spaces bucket $name in $region..."
  # Newer doctl versions support `doctl spaces buckets create`; older ones don't.
  if doctl spaces buckets create "$name" --region "$region" >/dev/null 2>&1; then
    echo "  ok"
  else
    echo "  doctl can't create the bucket (CLI version doesn't support it);"
    echo "  create '$name' in region '$region' via the dashboard."
  fi
}

create_or_skip_space "shithub-backups"    "$PRIMARY_REGION"
create_or_skip_space "shithub-backups-dr" "$DR_REGION"
create_or_skip_space "shithub-docs"       "$PRIMARY_REGION"

# --- 5. Move resources into the project ---
echo "assigning resources to project $PROJECT_NAME..."
doctl projects resources assign "$PROJECT_ID" \
  --resource "do:droplet:$APP_ID" \
  --resource "do:droplet:$DB_ID" \
  --resource "do:droplet:$BAK_ID" \
  --resource "do:droplet:$MON_ID" \
  --resource "do:volume:$VOL_ID" >/dev/null

# --- 6. Print the summary the operator needs for the inventory ---
cat <<SUMMARY

==============================================================
provisioned. summary for inventory:

Region (primary):     $PRIMARY_REGION
Region (DR):          $DR_REGION
Project:              $PROJECT_NAME (id $PROJECT_ID)

Droplets (public IPv4 → private IPv4):
SUMMARY

doctl compute droplet list --no-header --tag-name shithub \
  --format Name,PublicIPv4,PrivateIPv4 \
  | column -t

cat <<SUMMARY

Volume:               $VOL_NAME (id $VOL_ID; attached to shithub-app)

Spaces buckets:
  shithub-backups      ($PRIMARY_REGION) — endpoint: $PRIMARY_REGION.digitaloceanspaces.com
  shithub-backups-dr   ($DR_REGION)      — endpoint: $DR_REGION.digitaloceanspaces.com
  shithub-docs         ($PRIMARY_REGION, CDN target)

NEXT STEPS (manual, no doctl path):
  1. Generate Spaces access keys via dashboard:
       API → Spaces Keys → Generate New Key
       Name: shithub-prod-app
       Copy the secret immediately (shown once).
  2. Enable CDN on the shithub-docs bucket and set custom domain
       to docs.shithub.sh (DO will print a CNAME target).
  3. Set Namecheap DNS records:
       A     @     <shithub-app public IPv4>
       A     www   <shithub-app public IPv4>
       CNAME docs  <CDN target from step 2>
  4. Continue with SETUP-GUIDE.md Phase B5 (SSH-bootstrap).
==============================================================
SUMMARY
