#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later

set -euo pipefail

fail() {
  printf 'deploy-actions-runners: %s\n' "$*" >&2
  exit 1
}

note() {
  printf 'deploy-actions-runners: %s\n' "$*"
}

require_safe_token() {
  name="$1"
  value="$2"
  case "$value" in
    *[!A-Za-z0-9._:-]*)
      fail "$name contains unsupported characters"
      ;;
  esac
}

require_safe_ssh_target() {
  name="$1"
  value="$2"
  case "$value" in
    *[!A-Za-z0-9._:@-]*)
      fail "$name contains unsupported characters"
      ;;
  esac
}

require_safe_labels() {
  value="$1"
  case "$value" in
    *[!A-Za-z0-9_.\,-]*)
      fail "RUNNER_PREFLIGHT_LABELS contains unsupported characters"
      ;;
  esac
}

normalize_hosts() {
  printf '%s\n' "$1" |
    tr ',[:space:]' '\n' |
    sed '/^$/d'
}

ssh_target() {
  host="$1"
  case "$host" in
    *@*) printf '%s\n' "$host" ;;
    *) printf '%s@%s\n' "$runner_user" "$host" ;;
  esac
}

remote_preflight_target() {
  case "$preflight_host" in
    *@*) printf '%s\n' "$preflight_host" ;;
    *) printf '%s@%s\n' "$preflight_user" "$preflight_host" ;;
  esac
}

run_ssh() {
  ssh -o BatchMode=yes -o StrictHostKeyChecking=yes "$@"
}

expand_local_path() {
  value="$1"
  case "$value" in
    '~')
      [ -n "${HOME:-}" ] || fail "HOME is required to expand $value"
      printf '%s\n' "$HOME"
      ;;
    '~/'*)
      [ -n "${HOME:-}" ] || fail "HOME is required to expand $value"
      printf '%s/%s\n' "$HOME" "${value#\~/}"
      ;;
    '$HOME')
      [ -n "${HOME:-}" ] || fail "HOME is required to expand $value"
      printf '%s\n' "$HOME"
      ;;
    '$HOME/'*)
      [ -n "${HOME:-}" ] || fail "HOME is required to expand $value"
      printf '%s/%s\n' "$HOME" "${value#\$HOME/}"
      ;;
    *)
      printf '%s\n' "$value"
      ;;
  esac
}

relay_cleanup_target=""
relay_cleanup_dir=""

cleanup_relay_tmp() {
  if [ -n "$relay_cleanup_target" ] && [ -n "$relay_cleanup_dir" ]; then
    run_ssh "$relay_cleanup_target" "rm -rf '$relay_cleanup_dir'" >/dev/null 2>&1 || true
  fi
}

trap cleanup_relay_tmp EXIT

relay_target() {
  case "$relay_host" in
    *@*) printf '%s\n' "$relay_host" ;;
    *) printf '%s@%s\n' "$relay_user" "$relay_host" ;;
  esac
}

deploy_direct() {
  for host in "${runner_hosts[@]}"; do
    target="$(ssh_target "$host")"
    note "staging candidate on $target"
    if [ "$dry_run" = "true" ]; then
      note "dry-run: would stream $runner_bin to $target:$candidate"
      continue
    fi

    run_ssh "$target" "set -e; install -d -m 0750 /var/lib/shithubd-runner/binaries; cat > '$candidate'; chmod 0755 '$candidate'; '$candidate' version" <"$runner_bin"

    note "promoting candidate on $target"
    version_out="$(
      run_ssh "$target" "set -e; backup=/var/lib/shithubd-runner/binaries/shithubd-runner-before-${expected_commit}-\$(date -u +%Y%m%d%H%M%S); if [ -x /usr/local/bin/shithubd-runner ]; then cp /usr/local/bin/shithubd-runner \"\$backup\"; fi; install -o root -g root -m 0755 '$candidate' /usr/local/bin/shithubd-runner; systemctl restart shithubd-runner; systemctl is-active --quiet shithubd-runner; /usr/local/bin/shithubd-runner version"
    )"
    printf '%s\n' "$version_out"
    case "$(printf '%s\n' "$version_out" | tr '[:upper:]' '[:lower:]')" in
      *"$expected_commit_lower"*) ;;
      *) fail "$target restarted but version output did not contain $expected_commit" ;;
    esac
  done
}

run_remote_preflight() {
  if [ -z "$preflight_host" ]; then
    note "RUNNER_PREFLIGHT_HOST not set; skipping heartbeat preflight"
    return 0
  fi

  preflight_target="$(remote_preflight_target)"
  preflight_cmd="set -e; set -a; . /etc/shithub/web.env; set +a; /usr/local/bin/shithubd admin runner preflight --labels '$preflight_labels' --min-runners '$preflight_min' --max-heartbeat-age '$preflight_max_age' --expected-commit '$expected_commit' --output json"

  for attempt in $(seq 1 "$preflight_attempts"); do
    note "preflight attempt $attempt/$preflight_attempts on $preflight_target"
    if [ "$dry_run" = "true" ]; then
      note "dry-run: would run runner preflight on $preflight_target"
      return 0
    fi
    if run_ssh "$preflight_target" "$preflight_cmd"; then
      note "runner fleet preflight passed"
      return 0
    fi
    if [ "$attempt" -lt "$preflight_attempts" ]; then
      sleep "$preflight_sleep"
    fi
  done

  fail "runner fleet preflight did not pass after rollout"
}

deploy_via_relay() {
  relay="$(relay_target)"
  note "relaying runner deploy through $relay"

  if [ "$dry_run" = "true" ]; then
    note "dry-run: would upload $runner_bin and runner SSH credentials to $relay"
    note "dry-run: would deploy to ${#runner_hosts[@]} runner host(s) from relay"
    return 0
  fi

  [ -r "$runner_key_file" ] || fail "runner deploy key file is missing or unreadable: $runner_key_file"
  [ -r "$runner_known_hosts_file" ] || fail "runner known_hosts file is missing or unreadable: $runner_known_hosts_file"

  relay_tmp="$(run_ssh "$relay" "mktemp -d /tmp/shithub-runner-deploy.XXXXXX")"
  case "$relay_tmp" in
    /tmp/shithub-runner-deploy.*) ;;
    *) fail "relay returned unsafe temporary directory: $relay_tmp" ;;
  esac
  relay_cleanup_target="$relay"
  relay_cleanup_dir="$relay_tmp"

  run_ssh "$relay" "set -e; chmod 0700 '$relay_tmp'"
  run_ssh "$relay" "set -e; cat > '$relay_tmp/shithubd-runner'; chmod 0755 '$relay_tmp/shithubd-runner'" <"$runner_bin"
  run_ssh "$relay" "set -e; cat > '$relay_tmp/runner_key'; chmod 0600 '$relay_tmp/runner_key'" <"$runner_key_file"
  run_ssh "$relay" "set -e; cat > '$relay_tmp/known_hosts'; chmod 0600 '$relay_tmp/known_hosts'" <"$runner_known_hosts_file"
  printf '%s\n' "${runner_hosts[@]}" | run_ssh "$relay" "set -e; cat > '$relay_tmp/hosts'; chmod 0600 '$relay_tmp/hosts'"

  run_ssh "$relay" \
    "RUNNER_DEPLOY_USER='$runner_user' RUNNER_EXPECTED_COMMIT='$expected_commit' RUNNER_PREFLIGHT_LABELS='$preflight_labels' RUNNER_PREFLIGHT_MIN_RUNNERS='$preflight_min' RUNNER_PREFLIGHT_MAX_HEARTBEAT_AGE='$preflight_max_age' RUNNER_PREFLIGHT_ATTEMPTS='$preflight_attempts' RUNNER_PREFLIGHT_SLEEP_SECONDS='$preflight_sleep' RUNNER_CANDIDATE='$candidate' RELAY_TMP='$relay_tmp' bash -s" <<'REMOTE'
set -euo pipefail

fail_remote() {
  printf 'deploy-actions-runners relay: %s\n' "$*" >&2
  exit 1
}

note_remote() {
  printf 'deploy-actions-runners relay: %s\n' "$*"
}

ssh_base=(
  ssh
  -i "$RELAY_TMP/runner_key"
  -o UserKnownHostsFile="$RELAY_TMP/known_hosts"
  -o StrictHostKeyChecking=yes
  -o BatchMode=yes
)

expected_lower="$(printf '%s\n' "$RUNNER_EXPECTED_COMMIT" | tr '[:upper:]' '[:lower:]')"

while IFS= read -r host; do
  [ -n "$host" ] || continue
  case "$host" in
    *@*) target="$host" ;;
    *) target="${RUNNER_DEPLOY_USER}@${host}" ;;
  esac

  note_remote "staging candidate on $target"
  "${ssh_base[@]}" "$target" "set -e; install -d -m 0750 /var/lib/shithubd-runner/binaries; cat > '$RUNNER_CANDIDATE'; chmod 0755 '$RUNNER_CANDIDATE'; '$RUNNER_CANDIDATE' version" <"$RELAY_TMP/shithubd-runner"

  note_remote "promoting candidate on $target"
  version_out="$("${ssh_base[@]}" "$target" "set -e; backup=/var/lib/shithubd-runner/binaries/shithubd-runner-before-${RUNNER_EXPECTED_COMMIT}-\$(date -u +%Y%m%d%H%M%S); if [ -x /usr/local/bin/shithubd-runner ]; then cp /usr/local/bin/shithubd-runner \"\$backup\"; fi; install -o root -g root -m 0755 '$RUNNER_CANDIDATE' /usr/local/bin/shithubd-runner; systemctl restart shithubd-runner; systemctl is-active --quiet shithubd-runner; /usr/local/bin/shithubd-runner version")"
  printf '%s\n' "$version_out"
  case "$(printf '%s\n' "$version_out" | tr '[:upper:]' '[:lower:]')" in
    *"$expected_lower"*) ;;
    *) fail_remote "$target restarted but version output did not contain $RUNNER_EXPECTED_COMMIT" ;;
  esac
done <"$RELAY_TMP/hosts"

preflight_cmd=(
  /usr/local/bin/shithubd
  admin
  runner
  preflight
  --labels "$RUNNER_PREFLIGHT_LABELS"
  --min-runners "$RUNNER_PREFLIGHT_MIN_RUNNERS"
  --max-heartbeat-age "$RUNNER_PREFLIGHT_MAX_HEARTBEAT_AGE"
  --expected-commit "$RUNNER_EXPECTED_COMMIT"
  --output json
)

for attempt in $(seq 1 "$RUNNER_PREFLIGHT_ATTEMPTS"); do
  note_remote "preflight attempt $attempt/$RUNNER_PREFLIGHT_ATTEMPTS on relay host"
  if bash -c 'set -e; set -a; . /etc/shithub/web.env; set +a; exec "$@"' shithub-runner-preflight "${preflight_cmd[@]}"; then
    note_remote "runner fleet preflight passed"
    exit 0
  fi
  if [ "$attempt" -lt "$RUNNER_PREFLIGHT_ATTEMPTS" ]; then
    sleep "$RUNNER_PREFLIGHT_SLEEP_SECONDS"
  fi
done

fail_remote "runner fleet preflight did not pass after rollout"
REMOTE
}

runner_hosts_raw="${RUNNER_DEPLOY_HOSTS:-}"
[ -n "$runner_hosts_raw" ] || fail "RUNNER_DEPLOY_HOSTS is required"

runner_user="${RUNNER_DEPLOY_USER:-root}"
runner_bin="${RUNNER_DEPLOY_BIN:-bin/shithubd-runner}"
expected_commit="${RUNNER_EXPECTED_COMMIT:-$(git rev-parse --short HEAD)}"
expected_commit_lower="$(printf '%s\n' "$expected_commit" | tr '[:upper:]' '[:lower:]')"
preflight_host="${RUNNER_PREFLIGHT_HOST:-}"
preflight_user="${RUNNER_PREFLIGHT_USER:-root}"
preflight_labels="${RUNNER_PREFLIGHT_LABELS:-ubuntu-latest}"
preflight_min="${RUNNER_PREFLIGHT_MIN_RUNNERS:-}"
preflight_max_age="${RUNNER_PREFLIGHT_MAX_HEARTBEAT_AGE:-2m}"
preflight_attempts="${RUNNER_PREFLIGHT_ATTEMPTS:-18}"
preflight_sleep="${RUNNER_PREFLIGHT_SLEEP_SECONDS:-5}"
relay_host="${RUNNER_DEPLOY_RELAY_HOST:-}"
relay_user="${RUNNER_DEPLOY_RELAY_USER:-root}"
runner_key_file="$(expand_local_path "${RUNNER_DEPLOY_SSH_KEY_FILE:-${HOME:-}/.ssh/runner_deploy}")"
runner_known_hosts_file="$(expand_local_path "${RUNNER_DEPLOY_KNOWN_HOSTS_FILE:-${HOME:-}/.ssh/known_hosts}")"
dry_run="${RUNNER_DEPLOY_DRY_RUN:-false}"

[ -x "$runner_bin" ] || fail "runner binary is missing or not executable: $runner_bin"
require_safe_token "RUNNER_EXPECTED_COMMIT" "$expected_commit"
require_safe_token "RUNNER_DEPLOY_USER" "$runner_user"
require_safe_token "RUNNER_PREFLIGHT_USER" "$preflight_user"
require_safe_token "RUNNER_PREFLIGHT_MAX_HEARTBEAT_AGE" "$preflight_max_age"
require_safe_token "RUNNER_DEPLOY_RELAY_USER" "$relay_user"
require_safe_labels "$preflight_labels"
if [ -n "$relay_host" ]; then
  require_safe_ssh_target "RUNNER_DEPLOY_RELAY_HOST" "$relay_host"
fi

runner_hosts=()
while IFS= read -r host; do
  require_safe_ssh_target "RUNNER_DEPLOY_HOSTS entry" "$host"
  runner_hosts+=("$host")
done < <(normalize_hosts "$runner_hosts_raw")
[ "${#runner_hosts[@]}" -gt 0 ] || fail "RUNNER_DEPLOY_HOSTS did not contain any hosts"

if [ -z "$preflight_min" ]; then
  preflight_min="${#runner_hosts[@]}"
fi
case "$preflight_min" in
  ''|*[!0-9]*) fail "RUNNER_PREFLIGHT_MIN_RUNNERS must be numeric" ;;
esac
case "$preflight_attempts" in
  ''|*[!0-9]*) fail "RUNNER_PREFLIGHT_ATTEMPTS must be numeric" ;;
esac
case "$preflight_sleep" in
  ''|*[!0-9]*) fail "RUNNER_PREFLIGHT_SLEEP_SECONDS must be numeric" ;;
esac

suffix="${GITHUB_RUN_ID:-manual}-$(date -u +%Y%m%d%H%M%S)"
candidate="/var/lib/shithubd-runner/binaries/shithubd-runner-candidate-${expected_commit}-${suffix}"

note "deploying $runner_bin to ${#runner_hosts[@]} runner host(s); expected_commit=$expected_commit"

if [ -n "$relay_host" ]; then
  deploy_via_relay
else
  deploy_direct
  run_remote_preflight
fi
