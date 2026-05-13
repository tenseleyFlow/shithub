#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later

set -eu

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

fail() {
  printf 'audit-actions-public-runners: %s\n' "$*" >&2
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
  rg -q -- "$pattern" "$file" || fail "$desc not found in $file"
  ok "$desc"
}

require_file "docs/internal/actions-public-runners.md"
require_file "docs/internal/runbooks/actions-runner.md"
require_file "docs/internal/runbooks/runner-deploy.md"
require_file "deploy/doctl/provision-actions-runner-pool.sh"
require_file "deploy/doctl/generate-actions-runner-inventory.sh"
require_file "deploy/runner-config/firewall.sh.j2"
require_file "deploy/runner-config/dnsmasq.conf.j2"
require_file "deploy/runner-config/seccomp.json"

require_grep 'WHEN COALESCE\(sp\.actions_enabled, true\) = false THEN false' \
  "internal/actions/queries/actions_policy.sql" \
  "site kill switch in effective policy"
require_grep 'WHEN COALESCE\(sp\.actions_enabled, true\) = false THEN false' \
  "internal/actions/queries/workflow_jobs.sql" \
  "site kill switch in runner claim"
require_grep 'TestEvaluateTrigger_SiteDisableOverridesRepoEnable' \
  "internal/actions/policy/policy_test.go" \
  "enqueue-time site kill switch test"
require_grep 'TestRunnerHeartbeatSiteDisableOverridesRepoEnable' \
  "internal/web/handlers/api/runners_test.go" \
  "claim-time site kill switch test"
require_grep 'TestRunnerHeartbeatRespectsRepoConcurrencyCap' \
  "internal/web/handlers/api/runners_test.go" \
  "repo concurrency claim test"
require_grep 'TestRunnerHeartbeatRespectsOwnerConcurrencyCap' \
  "internal/web/handlers/api/runners_test.go" \
  "owner concurrency claim test"

require_grep '--cap-drop=ALL' "internal/runner/engine/docker.go" "cap drop in Docker engine"
require_grep '--read-only' "internal/runner/engine/docker.go" "read-only rootfs in Docker engine"
require_grep '--security-opt=no-new-privileges' "internal/runner/engine/docker.go" "no-new-privileges in Docker engine"
require_grep 'seccomp=' "internal/runner/engine/docker.go" "seccomp profile in Docker engine"
require_grep '--user' "internal/runner/engine/docker.go" "non-root container user in Docker engine"
require_grep 'rejects direct-IP' "docs/internal/runbooks/runner-deploy.md" "direct-IP egress runbook note"
require_grep '-j REJECT' "deploy/runner-config/firewall.sh.j2" "runner firewall default reject"
require_grep 'Do not put runner tokens' "deploy/doctl/actions-runner-cloud-init.yaml" "no-secret cloud-init warning"

require_grep 'FeatureOrgActionsMinutesQuota' "internal/entitlements/entitlements.go" "actions minutes entitlement key"
require_grep 'LimitOrgActionsMinutesQuota' "internal/entitlements/entitlements.go" "actions minutes limit key"
require_grep 'no concrete number until' "docs/internal/actions-public-runners.md" "billing-metering caveat"
require_grep 'controlled dogfood, not broad public GA' "docs/internal/actions-public-runners.md" "public runner rollout status"

ok "S41j-6 public runner readiness static audit complete"
