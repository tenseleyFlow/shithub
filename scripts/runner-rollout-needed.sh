#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later

set -eu

if [ "$#" -ne 2 ]; then
  printf 'usage: %s <base-ref> <head-ref>\n' "$0" >&2
  exit 2
fi

base_ref="$1"
head_ref="$2"
root="$(git rev-parse --show-toplevel)"
cd "$root"

changed_files="$(git diff --name-only "$base_ref" "$head_ref")"
if [ -z "$changed_files" ]; then
  printf 'false\n'
  exit 0
fi

tmp="${TMPDIR:-/tmp}/shithub-runner-rollout-paths.$$"
trap 'rm -f "$tmp"' EXIT

GOCACHE="${GOCACHE:-${TMPDIR:-/tmp}/shithub-go-cache}" \
  go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./cmd/shithubd-runner |
  while IFS= read -r dir; do
    case "$dir" in
      "$root"/*)
        rel="${dir#"$root"/}"
        printf '%s/\n' "$rel"
        ;;
    esac
  done |
  sort -u >"$tmp"

cat >>"$tmp" <<'PATHS'
.github/workflows/deploy-runners.yml
Makefile
go.mod
go.sum
scripts/deploy-actions-runners.sh
scripts/runner-rollout-needed.sh
PATHS

while IFS= read -r changed; do
  [ -n "$changed" ] || continue
  while IFS= read -r owned; do
    [ -n "$owned" ] || continue
    case "$owned" in
      */)
        case "$changed" in
          "$owned"*) printf 'true\n'; exit 0 ;;
        esac
        ;;
      *)
        if [ "$changed" = "$owned" ]; then
          printf 'true\n'
          exit 0
        fi
        ;;
    esac
  done <"$tmp"
done <<EOF
$changed_files
EOF

printf 'false\n'
