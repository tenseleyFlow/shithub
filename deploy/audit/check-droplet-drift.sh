#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Read-only audit: compare files the ansible roles claim to manage
# against what's actually on shithub-prod. Reports drift; never
# writes. Run after every PR that touches deploy/ansible/ to confirm
# what (if anything) needs to be pushed to the droplet manually.
#
# This is a stopgap until we pick a long-term strategy (see issue #38).
#
# Usage:
#   deploy/audit/check-droplet-drift.sh
#
# Exits 0 if no drift, 1 if drift detected, 2 on infra failure.

set -uo pipefail

HOST="${SHITHUB_HOST:-root@shithub.sh}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Map each managed path to its source in the repo (when there's a
# direct copy: with no templating). For template: actions we just
# check existence and mtime — comparing rendered output requires
# inventory variables we don't have locally.
#
# Format: <droplet path>::<source in repo or ::TEMPLATE>
declare -a MANAGED=(
  "/usr/local/bin/shithub-backup-daily::deploy/postgres/backup-daily.sh"
  "/usr/local/bin/shithub-spaces-sync::deploy/spaces/sync-cross-region.sh"
  "/usr/local/bin/shithub-pg-archive::deploy/postgres/archive_command.sh"
  "/usr/local/bin/shithub-verify-wal-archive::deploy/postgres/verify-wal-archive.sh"
  "/usr/local/bin/shithub-aide-check::deploy/ansible/roles/base/files/shithub-aide-check.sh"
  "/usr/local/bin/shithub-ssh-authkeys::deploy/ansible/roles/shithubd/files/shithub-ssh-authkeys"
  "/var/lib/git/git-shell-commands/shithubd::deploy/ansible/roles/shithubd/files/git-shell-commands-shithubd"
  "/etc/rclone-shithub.conf::TEMPLATE"
  "/etc/alloy/config.alloy::TEMPLATE"
  "/etc/alloy/credentials.env::TEMPLATE"
  "/etc/systemd/system/alloy.service.d/shithub.conf::TEMPLATE"
  "/etc/postgresql/16/main/conf.d/99_shithub_archive.conf::TEMPLATE"
  # Known drift, tracked deliberately: the box runs Debian defaults
  # here. deploy/ansible/roles/postgres/templates/postgresql.conf.j2
  # has never been applied (see docs/internal/db.md); applying it needs
  # a Postgres restart in the 05:15-06:00 window, not a `make deploy`.
  "/etc/postgresql/16/main/postgresql.conf::TEMPLATE"
  "/etc/aide/aide.conf.d/99_shithub_exclude::deploy/ansible/roles/base/files/aide-shithub.conf"
  "/etc/cron.daily/aide::TEMPLATE"
  "/etc/caddy/Caddyfile::TEMPLATE"
  "/etc/fail2ban/jail.d/shithub.local::TEMPLATE"
  "/etc/fail2ban/filter.d/shithubd-auth.conf::TEMPLATE"
  "/etc/systemd/system/shithubd-web.service::TEMPLATE"
  "/etc/systemd/system/shithubd-worker.service::TEMPLATE"
  # Known drift: web.env carries a hand-edited Stripe block that is
  # not in web.env.j2. A `make deploy` re-renders this file from the
  # template and WILL drop those keys — see KNOWN_DRIFT below.
  "/etc/shithub/web.env::TEMPLATE"
  "/etc/shithub/worker.env::TEMPLATE"
  "/etc/ssh/sshd_config::TEMPLATE"
)

# Paths whose drift is known and accepted. Reported as KNOWN rather
# than silently passing: the point is that the next operator reads the
# reason instead of rediscovering it.
declare -a KNOWN_DRIFT=(
  "/etc/postgresql/16/main/postgresql.conf::postgresql.conf.j2 has never been applied; needs a restart, see docs/internal/db.md"
  "/etc/shithub/web.env::hand-edited Stripe block not present in web.env.j2; a full deploy re-renders this file and drops it"
)

known_drift_note() {
  local entry
  for entry in "${KNOWN_DRIFT[@]}"; do
    if [ "${entry%%::*}" = "$1" ]; then
      printf '%s' "${entry##*::}"
      return 0
    fi
  done
  return 1
}

DRIFT_COUNT=0

# Build a single-shot SSH script that returns md5 + stat for every
# managed path. Avoids one ssh round-trip per file.
remote_script="
for path in"
for entry in "${MANAGED[@]}"; do
  remote_script+=" '${entry%%::*}'"
done
remote_script+=$'; do\n'
remote_script+=$'  if [ -f "$path" ]; then\n'
remote_script+=$'    md5=$(md5sum "$path" 2>/dev/null | cut -d" " -f1)\n'
remote_script+=$'    stat=$(stat -c "%a %U:%G %s" "$path" 2>/dev/null)\n'
remote_script+=$'    printf "%s|EXISTS|%s|%s\\n" "$path" "$md5" "$stat"\n'
remote_script+=$'  else\n'
remote_script+=$'    printf "%s|MISSING||\\n" "$path"\n'
remote_script+=$'  fi\n'
remote_script+=$'done\n'

if ! remote_output=$(ssh -o BatchMode=yes "$HOST" "$remote_script" 2>&1); then
  echo "ssh to $HOST failed:" >&2
  echo "$remote_output" >&2
  exit 2
fi

printf "%-60s  %-10s  %s\n" "PATH" "STATUS" "DETAIL"
printf "%-60s  %-10s  %s\n" "----" "------" "------"

while IFS='|' read -r dpath status remote_md5 remote_stat; do
  # Find the matching entry to look up the source
  src=""
  for entry in "${MANAGED[@]}"; do
    if [ "${entry%%::*}" = "$dpath" ]; then
      src="${entry##*::}"
      break
    fi
  done

  if [ "$status" = "MISSING" ]; then
    printf "%-60s  \033[33m%-10s\033[0m  (not on droplet)\n" "$dpath" "MISSING"
    DRIFT_COUNT=$((DRIFT_COUNT + 1))
    continue
  fi

  if [ "$src" = "TEMPLATE" ]; then
    if note=$(known_drift_note "$dpath"); then
      printf "%-60s  \033[33m%-10s\033[0m  %s\n" "$dpath" "KNOWN" "$note"
    else
      printf "%-60s  \033[36m%-10s\033[0m  %s  (template — manual check)\n" "$dpath" "TEMPLATE" "$remote_stat"
    fi
    continue
  fi

  if [ ! -f "$REPO_ROOT/$src" ]; then
    printf "%-60s  \033[31m%-10s\033[0m  source missing: %s\n" "$dpath" "ERROR" "$src"
    DRIFT_COUNT=$((DRIFT_COUNT + 1))
    continue
  fi

  local_md5=$(md5sum "$REPO_ROOT/$src" | cut -d' ' -f1)
  if [ "$local_md5" = "$remote_md5" ]; then
    printf "%-60s  \033[32m%-10s\033[0m  %s\n" "$dpath" "OK" "$remote_stat"
  else
    printf "%-60s  \033[31m%-10s\033[0m  repo=%s droplet=%s\n" "$dpath" "DRIFT" "${local_md5:0:8}" "${remote_md5:0:8}"
    DRIFT_COUNT=$((DRIFT_COUNT + 1))
  fi
done <<< "$remote_output"

echo ""
if [ "$DRIFT_COUNT" -gt 0 ]; then
  echo "Drift detected: $DRIFT_COUNT file(s) need attention."
  echo "TEMPLATE rows are not auto-checked — eyeball the timestamps and confirm by hand."
  exit 1
fi
echo "No drift detected on copy: files. TEMPLATE rows still need manual review."
echo "KNOWN rows are accepted drift with a reason attached — do not 'fix' one"
echo "by running a full deploy without reading the note."
exit 0
