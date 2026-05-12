#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later

set -eu

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

fail() {
  printf 'audit-actions-ga: %s\n' "$*" >&2
  exit 1
}

ok() {
  printf 'ok: %s\n' "$*"
}

require_file() {
  [ -f "$1" ] || fail "missing required file: $1"
  ok "found $1"
}

require_grep() {
  pattern="$1"
  file="$2"
  desc="$3"
  rg -q "$pattern" "$file" || fail "$desc not found in $file"
  ok "$desc"
}

require_file ".shithub/workflows/checkout-canary.yml"
require_file "bench/fixtures/actions/checkout-canary.yml"
require_file "bench/k6/actions-load.js"
require_file "deploy/monitoring/grafana/dashboards/actions.json"
require_file "deploy/monitoring/prometheus/rules.yml"
require_file "deploy/runner-config/firewall.sh.j2"
require_file "deploy/runner-config/dnsmasq.conf.j2"
require_file "deploy/runner-config/seccomp.json"
require_file "docs/internal/actions-ga-readiness.md"
require_file "docs/internal/runbooks/actions.md"
require_file "docs/internal/runbooks/runner-deploy.md"
require_file "docs/public/user/actions.md"
require_file "docs/public/api/actions.md"

uses_hits="$(rg -n '^[[:space:]-]*uses:[[:space:]]*' .shithub/workflows -g '*.yml' -g '*.yaml' || true)"
printf '%s\n' "$uses_hits" | while IFS= read -r hit; do
  [ -n "$hit" ] || continue
  ref="$(printf '%s' "$hit" | sed -E 's/.*uses:[[:space:]]*//; s/[[:space:]]+#.*$//; s/^"//; s/"$//; s/^[[:space:]]*//; s/[[:space:]]*$//')"
  ref="${ref#\'}"
  ref="${ref%\'}"
  case "$ref" in
    actions/checkout@v4|shithub/upload-artifact@v1|shithub/download-artifact@v1)
      ;;
    *)
      fail "unsupported .shithub workflow uses alias $ref in $hit"
      ;;
  esac
done
ok ".shithub workflows use only v1-supported aliases"

require_grep 'actions/setup-go@v5' ".github/workflows/ci.yml" "GitHub CI still documents setup-go dependency"
require_grep 'golangci/golangci-lint-action@v8' ".github/workflows/ci.yml" "GitHub CI still documents golangci action dependency"
require_grep 'Do not move `.github/workflows/ci.yml`' "docs/internal/actions-ga-readiness.md" "dogfood decision"

for alert in \
  ActionsRunnerHeartbeatStale \
  ActionsQueueDepthHigh \
  ActionsRunDurationP99Regressed \
  ActionsLogScrubberPossiblyMissing
do
  require_grep "$alert" "deploy/monitoring/prometheus/rules.yml" "alert $alert"
done

for metric in \
  shithub_actions_queue_depth \
  shithub_actions_active \
  shithub_actions_runner_heartbeat_age_seconds \
  shithub_actions_run_duration_seconds \
  shithub_actions_log_chunk_bytes_total
do
  require_grep "$metric" "docs/internal/runbooks/observability.md" "observability doc metric $metric"
done

require_grep 'runner_jwt_used' "docs/internal/actions-schema.md" "runner JWT replay table documentation"
require_grep 'workflow_job_secret_masks' "docs/internal/actions-schema.md" "claim-time mask table documentation"
require_grep 'direct-IP' "docs/internal/runbooks/runner-deploy.md" "direct-IP egress mitigation"
require_grep 'checkout token leaked into argv' "internal/runner/engine/docker_test.go" "checkout-token argv regression test"
require_grep 'checkout token push unexpectedly succeeded' "internal/web/handlers/githttp/githttp_test.go" "checkout-token push denial test"
require_grep 'TestEval_GithubAliasIsTainted' "internal/actions/expr/eval_test.go" "github alias taint test"
require_grep 'Actions workflow API' "docs/public/SUMMARY.md" "public Actions API docs link"
require_grep '\[Actions\]\(\./user/actions\.md\)' "docs/public/SUMMARY.md" "public Actions user docs link"

ok "S41h Actions pre-GA static audit packet complete"
