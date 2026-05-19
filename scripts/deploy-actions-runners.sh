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

runner_hosts_raw="${RUNNER_DEPLOY_HOSTS:-}"
[ -n "$runner_hosts_raw" ] || fail "RUNNER_DEPLOY_HOSTS is required"

runner_user="${RUNNER_DEPLOY_USER:-root}"
runner_bin="${RUNNER_DEPLOY_BIN:-bin/shithubd-runner}"
expected_commit="${RUNNER_EXPECTED_COMMIT:-$(git rev-parse --short HEAD)}"
preflight_host="${RUNNER_PREFLIGHT_HOST:-}"
preflight_user="${RUNNER_PREFLIGHT_USER:-root}"
preflight_labels="${RUNNER_PREFLIGHT_LABELS:-ubuntu-latest}"
preflight_min="${RUNNER_PREFLIGHT_MIN_RUNNERS:-}"
preflight_max_age="${RUNNER_PREFLIGHT_MAX_HEARTBEAT_AGE:-2m}"
preflight_attempts="${RUNNER_PREFLIGHT_ATTEMPTS:-18}"
preflight_sleep="${RUNNER_PREFLIGHT_SLEEP_SECONDS:-5}"
dry_run="${RUNNER_DEPLOY_DRY_RUN:-false}"

[ -x "$runner_bin" ] || fail "runner binary is missing or not executable: $runner_bin"
require_safe_token "RUNNER_EXPECTED_COMMIT" "$expected_commit"
require_safe_token "RUNNER_DEPLOY_USER" "$runner_user"
require_safe_token "RUNNER_PREFLIGHT_USER" "$preflight_user"
require_safe_token "RUNNER_PREFLIGHT_MAX_HEARTBEAT_AGE" "$preflight_max_age"
require_safe_labels "$preflight_labels"

runner_hosts=()
while IFS= read -r host; do
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
  expected_commit_lower="$(printf '%s\n' "$expected_commit" | tr '[:upper:]' '[:lower:]')"
  case "$(printf '%s\n' "$version_out" | tr '[:upper:]' '[:lower:]')" in
    *"$expected_commit_lower"*) ;;
    *) fail "$target restarted but version output did not contain $expected_commit" ;;
  esac
done

if [ -z "$preflight_host" ]; then
  note "RUNNER_PREFLIGHT_HOST not set; skipping heartbeat preflight"
  exit 0
fi

preflight_target="$(remote_preflight_target)"
preflight_cmd="set -e; set -a; . /etc/shithub/web.env; set +a; /usr/local/bin/shithubd admin runner preflight --labels '$preflight_labels' --min-runners '$preflight_min' --max-heartbeat-age '$preflight_max_age' --expected-commit '$expected_commit' --output json"

for attempt in $(seq 1 "$preflight_attempts"); do
  note "preflight attempt $attempt/$preflight_attempts on $preflight_target"
  if [ "$dry_run" = "true" ]; then
    note "dry-run: would run runner preflight on $preflight_target"
    exit 0
  fi
  if run_ssh "$preflight_target" "$preflight_cmd"; then
    note "runner fleet preflight passed"
    exit 0
  fi
  if [ "$attempt" -lt "$preflight_attempts" ]; then
    sleep "$preflight_sleep"
  fi
done

fail "runner fleet preflight did not pass after rollout"
