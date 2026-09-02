#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Static checks on the shipped systemd units. deploy/redeploy.sh
# installs these verbatim on every push to trunk, so a unit that
# silently loses its memory ceiling reaches production without anyone
# reading the diff — which is exactly how the box ended up with no
# limit on any unit before the 2026-09-02 availability sitrep.
#
# Deliberately not `systemd-analyze verify`: that resolves ExecStart
# paths and EnvironmentFile= against the *local* filesystem, so it
# fails on any machine that isn't the deploy target. These are the
# invariants we actually care about regressing.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

units_dir=deploy/systemd
fail=0

err() {
  echo "lint-systemd-units: $*" >&2
  fail=1
}

# directive <file> <key> -> prints the value, empty if unset
directive() {
  sed -nE "s/^[[:space:]]*$2=(.*)$/\1/p" "$1" | tail -1
}

# to_mib <value like 512M | 2000M | 1200MiB | 1G> -> integer MiB
to_mib() {
  local v="$1" n unit
  n=$(printf '%s' "$v" | sed -E 's/[^0-9].*$//')
  unit=$(printf '%s' "$v" | sed -E 's/^[0-9]+//')
  case "$unit" in
    ""|M|MiB|MB) echo "$n" ;;
    G|GiB|GB)    echo $((n * 1024)) ;;
    K|KiB|KB)    echo $((n / 1024)) ;;
    *)           echo "" ;;
  esac
}

for f in "$units_dir"/*.service "$units_dir"/*.timer; do
  [ -e "$f" ] || continue
  if ! grep -q '^\[Unit\]' "$f"; then
    err "$f: missing [Unit] section"
  fi
  # Trailing whitespace in a systemd value is significant and always a
  # typo when it shows up in a directive line.
  if grep -nE '^[A-Za-z]+=.*[[:space:]]$' "$f" >/dev/null; then
    err "$f: directive with trailing whitespace"
  fi
done

# Both long-running units must be memory-bounded, and MemoryHigh (the
# throttle-and-reclaim threshold) must sit strictly below MemoryMax
# (the kill threshold) or the throttle never engages.
for unit in shithubd-web shithubd-worker; do
  f="$units_dir/$unit.service"
  [ -e "$f" ] || { err "$f: missing"; continue; }

  high=$(directive "$f" MemoryHigh)
  max=$(directive "$f" MemoryMax)

  [ -n "$high" ] || err "$unit: MemoryHigh is unset"
  [ -n "$max" ] || err "$unit: MemoryMax is unset"

  if [ -n "$high" ] && [ -n "$max" ]; then
    high_mib=$(to_mib "$high")
    max_mib=$(to_mib "$max")
    if [ -z "$high_mib" ] || [ -z "$max_mib" ]; then
      err "$unit: unparseable MemoryHigh=$high / MemoryMax=$max"
    elif [ "$high_mib" -ge "$max_mib" ]; then
      err "$unit: MemoryHigh=$high must be below MemoryMax=$max"
    fi
  fi
done

# The web process is the one with a 0.7 GB static template heap, so it
# also needs GOMEMLIMIT (a soft target the Go GC honours — the cgroup
# ceiling alone just converts an OOM kill into a different OOM kill)
# and it must be the last thing the kernel picks under global pressure.
web="$units_dir/shithubd-web.service"
gomemlimit=$(sed -nE 's/^[[:space:]]*Environment=GOMEMLIMIT=(.*)$/\1/p' "$web" | tail -1)
[ -n "$gomemlimit" ] || err "shithubd-web: Environment=GOMEMLIMIT is unset"

if [ -n "$gomemlimit" ]; then
  go_mib=$(to_mib "$gomemlimit")
  web_high_mib=$(to_mib "$(directive "$web" MemoryHigh)")
  if [ -z "$go_mib" ]; then
    err "shithubd-web: unparseable GOMEMLIMIT=$gomemlimit"
  elif [ -n "$web_high_mib" ] && [ "$go_mib" -ge "$web_high_mib" ]; then
    err "shithubd-web: GOMEMLIMIT=$gomemlimit must be below MemoryHigh"
  fi
fi

# Matched as a literal negative integer rather than compared
# numerically: `[ "$oom" -ge 0 ]` on a non-numeric value errors out
# instead of returning false, which would let garbage through.
oom=$(directive "$web" OOMScoreAdjust)
if ! printf '%s' "$oom" | grep -qE '^-[0-9]+$'; then
  err "shithubd-web: OOMScoreAdjust must be set and negative (got '${oom:-unset}')"
fi

# Housekeeping must not land in the 03:17 backup window or on top of
# the 06:00 AIDE scan. See docs/internal/runbooks/backups.md.
timer="$units_dir/shithubd-cron.timer"
oncalendar=$(directive "$timer" OnCalendar)
hour=$(printf '%s' "$oncalendar" | sed -nE 's/^.* ([0-9]{2}):[0-9]{2}:[0-9]{2}.*$/\1/p')
if [ -z "$hour" ]; then
  err "shithubd-cron.timer: could not parse OnCalendar='$oncalendar'"
elif [ "$hour" -eq 3 ] || [ "$hour" -eq 6 ]; then
  err "shithubd-cron.timer: OnCalendar hour $hour collides with the backup (03:17) or AIDE (06:00) window"
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "lint-systemd-units: ok"
