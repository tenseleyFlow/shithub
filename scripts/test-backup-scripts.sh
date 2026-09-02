#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Functional test for the two backup scripts that cron runs on the app
# box. Neither is covered by `go test`, and both were silently doing
# nothing observable for four months (0-byte logs, no heartbeat), so
# the invariants worth pinning are:
#
#   1. a successful run writes a heartbeat file the metrics collector
#      can read (epoch seconds), and logs a start + status=ok line;
#   2. a failed rclone exits NON-ZERO and writes NO heartbeat — a
#      stale heartbeat must never be refreshed by a broken run;
#   3. rclone's own chatter stays out of the cron-redirected stream.
#
# Everything external (rclone, sudo, pg_dump, pg_restore) is stubbed
# on PATH, so this runs anywhere with bash and no Postgres.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

BACKUP=deploy/postgres/backup-daily.sh
SYNC=deploy/spaces/sync-cross-region.sh

fails=0
ok()   { printf '  ok   %s\n' "$*"; }
bad()  { printf '  FAIL %s\n' "$*" >&2; fails=$((fails + 1)); }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# --- stubs -----------------------------------------------------------
bin="$work/bin"
mkdir -p "$bin"

cat > "$bin/sudo" <<'EOF'
#!/usr/bin/env bash
[ "$1" = "-u" ] && shift 2
exec "$@"
EOF

# Writes an empty file wherever --file= points, so pg_restore --list
# and the retention glob have something to chew on.
cat > "$bin/pg_dump" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in --file=*) : > "${a#--file=}" ;; esac
done
EOF

cat > "$bin/pg_restore" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

# STUB_RCLONE_EXIT lets a case force the failure path.
cat > "$bin/rclone" <<'EOF'
#!/usr/bin/env bash
echo "rclone-chatter: $*"
exit "${STUB_RCLONE_EXIT:-0}"
EOF

chmod +x "$bin"/*
export PATH="$bin:$PATH"

# --- helpers ---------------------------------------------------------

# heartbeat_is_epoch <file> -> 0 when the file holds a plausible unix
# timestamp (10 digits, this century).
heartbeat_is_epoch() {
  local v
  v="$(cat "$1" 2>/dev/null || true)"
  [[ "$v" =~ ^[0-9]{10}$ ]] && [ "$v" -gt 1600000000 ]
}

# --- backup-daily ----------------------------------------------------

echo "backup-daily.sh"

run_backup() {
  local case_dir="$1"
  mkdir -p "$case_dir"
  SHITHUB_BACKUP_LOCAL="$case_dir/dumps" \
  SHITHUB_BACKUP_HEARTBEAT="$case_dir/heartbeat" \
    "$BACKUP" > "$case_dir/cron.log" 2>&1
}

c="$work/backup-ok"
if run_backup "$c"; then
  ok "exits 0 on success"
else
  bad "exits 0 on success (got $?)"
fi
grep -q 'backup-daily start' "$c/cron.log" \
  && ok "logs a start line" || bad "logs a start line"
grep -q 'backup-daily end status=ok exit=0' "$c/cron.log" \
  && ok "logs status=ok" || bad "logs status=ok"
if heartbeat_is_epoch "$c/heartbeat"; then
  ok "writes an epoch-seconds heartbeat"
else
  bad "writes an epoch-seconds heartbeat (got '$(cat "$c/heartbeat" 2>/dev/null)')"
fi
[ -e "$c/heartbeat.tmp" ] && bad "leaves no heartbeat temp file"

c="$work/backup-fail"
mkdir -p "$c"
# Pre-seed a heartbeat from an earlier good run; a failed run must not
# touch it.
echo 1600000001 > "$c/heartbeat"
if STUB_RCLONE_EXIT=7 run_backup "$c"; then
  bad "exits non-zero when rclone fails"
else
  rc=$?
  [ "$rc" -eq 7 ] && ok "propagates the rclone exit status ($rc)" \
                  || ok "exits non-zero when rclone fails ($rc)"
fi
grep -q 'backup-daily end status=FAILED' "$c/cron.log" \
  && ok "logs status=FAILED" || bad "logs status=FAILED"
[ "$(cat "$c/heartbeat")" = "1600000001" ] \
  && ok "leaves the previous heartbeat untouched on failure" \
  || bad "leaves the previous heartbeat untouched on failure"

# --- sync-cross-region -----------------------------------------------

echo "sync-cross-region.sh"

run_sync() {
  local case_dir="$1"
  mkdir -p "$case_dir"
  SHITHUB_SPACES_SYNC_LOG="$case_dir/spaces-sync.log" \
  SHITHUB_SPACES_SYNC_HEARTBEAT="$case_dir/heartbeat" \
    "$SYNC" > "$case_dir/cron.log" 2>&1
}

c="$work/sync-ok"
if run_sync "$c"; then
  ok "exits 0 on success"
else
  bad "exits 0 on success (got $?)"
fi
grep -q 'spaces-sync start' "$c/cron.log" \
  && ok "start line reaches the cron-redirected stream" \
  || bad "start line reaches the cron-redirected stream"
grep -q 'spaces-sync end status=ok exit=0' "$c/cron.log" \
  && ok "status=ok reaches the cron-redirected stream" \
  || bad "status=ok reaches the cron-redirected stream"
grep -q 'spaces-sync end status=ok exit=0' "$c/spaces-sync.log" \
  && ok "status=ok also reaches the script's own log" \
  || bad "status=ok also reaches the script's own log"
grep -q 'rclone-chatter' "$c/spaces-sync.log" \
  && ok "rclone output lands in the script's own log" \
  || bad "rclone output lands in the script's own log"
grep -q 'rclone-chatter' "$c/cron.log" \
  && bad "rclone output stays out of the cron log" \
  || ok "rclone output stays out of the cron log"
if heartbeat_is_epoch "$c/heartbeat"; then
  ok "writes an epoch-seconds heartbeat"
else
  bad "writes an epoch-seconds heartbeat"
fi

c="$work/sync-fail"
mkdir -p "$c"
if STUB_RCLONE_EXIT=3 run_sync "$c"; then
  bad "exits non-zero when rclone fails"
else
  ok "exits non-zero when rclone fails ($?)"
fi
grep -q 'spaces-sync end status=FAILED' "$c/cron.log" \
  && ok "logs status=FAILED" || bad "logs status=FAILED"
[ -e "$c/heartbeat" ] && bad "writes no heartbeat on failure" \
                      || ok "writes no heartbeat on failure"

echo ""
if [ "$fails" -gt 0 ]; then
  echo "test-backup-scripts: $fails assertion(s) failed" >&2
  exit 1
fi
echo "test-backup-scripts: ok"
