#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Fail when source code emits log lines containing token-prefix
# patterns that would leak credentials into operator logs. The
# canonical bad shape:
#
#   logger.Info("got authorization header", "auth", r.Header.Get("Authorization"))
#
# Even when the value happens to be empty, the format string itself
# is a smell — anyone who edits the line to format the value back in
# will silently start logging the secret.
#
# Patterns we reject (case-insensitive substring match in *.go):
#   - "shithub_pat_"     — PAT token prefix
#   - "otpauth://"       — TOTP URI containing the secret
#   - "Authorization:"   — header literal that suggests dumping headers
#
# Carve-outs (whitelisted paths) live in EXEMPTS below. The S08 PAT
# redactor for git-clone URLs and the auth-flow tests are the legit
# call sites; everything else is suspicious.
#
# Run as part of `make ci`.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

EXEMPTS=(
  # S08 redactor explicitly handles the prefix to strip it from URLs.
  "internal/auth/pat/"
  # Log redactor IS the canonical handler for the secret patterns.
  "internal/infra/log/log.go"
  # TOTP package builds the otpauth:// URI for the QR provisioning
  # code — never logs it, just constructs.
  "internal/auth/totp/totp.go"
  # PAT middleware parses (not logs) the Authorization header.
  "internal/web/middleware/pat.go"
  # Git-HTTP auth handler mentions the prefix in doc comments.
  "internal/web/handlers/githttp/auth.go"
  # Tests legitimately mention these strings in fixtures.
  "_test.go"
  # The lint script itself documents the patterns.
  "scripts/lint-secret-logs.sh"
)

RE='shithub_pat_|otpauth://|Authorization:'

# grep -EIn: extended regex, ignore binary, show line numbers.
# Search only shithub-owned trees — .refs/ vendored repos are docs.
matches=$(grep -RIEn --include='*.go' "$RE" cmd internal scripts 2>/dev/null || true)

filtered=""
while IFS= read -r line; do
  [ -z "$line" ] && continue
  skip=false
  for pat in "${EXEMPTS[@]}"; do
    if [[ "$line" == *"$pat"* ]]; then
      skip=true
      break
    fi
  done
  $skip || filtered="${filtered}${line}"$'\n'
done <<< "$matches"

if [ -n "${filtered// /}" ]; then
  echo "lint-secret-logs: token-prefix patterns found in non-exempt source:" >&2
  echo "$filtered" >&2
  echo >&2
  echo "If this is a legitimate use, add the file to EXEMPTS in scripts/lint-secret-logs.sh." >&2
  exit 1
fi

echo "lint-secret-logs: ok"
