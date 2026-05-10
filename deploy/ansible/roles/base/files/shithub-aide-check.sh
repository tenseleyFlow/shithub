#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Nightly AIDE wrapper. Runs `aide --check` and:
#   - on no changes: silent (cron mail isn't sent)
#   - on changes:    appends the diff to /var/log/shithub/aide.log
#                    AND emits a tagged systemd journal record so the
#                    operator can `journalctl -t shithub-aide`.
#
# Email delivery is intentionally not wired up yet: the droplet has
# no MTA + the project's outbound SMTP (Postmark) is approval-gated.
# Once Postmark is approved end-to-end, swap the journal emit for a
# curl POST to https://api.postmarkapp.com/email — see the runbook
# at docs/internal/runbooks/aide.md.

set -uo pipefail   # NOT -e: aide --check exits non-zero on diff, which
                    # is the expected, non-fatal "you have alerts" signal.

LOG=/var/log/shithub/aide.log
mkdir -p "$(dirname "$LOG")"

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }

OUT="$(aide --check 2>&1)"
RC=$?

case "$RC" in
        0)
                # No changes — be silent. Touch a heartbeat file so the
                # operator can confirm the cron actually ran today.
                date -u +%Y-%m-%dT%H:%M:%SZ > /var/run/shithub-aide.last-clean
                exit 0
                ;;
        1|2|3|4|5|6|7)
                # AIDE encodes which categories changed in the exit code
                # (added/removed/changed file bits OR'd together). Any
                # non-zero is operator-visible by design.
                {
                        echo "[$(ts)] aide reported changes (rc=$RC)"
                        echo "----------------------------------------"
                        echo "$OUT"
                        echo "----------------------------------------"
                } >> "$LOG"
                # systemd-cat tags the journal so `journalctl -t shithub-aide`
                # filters cleanly. Priority warning so it shows up in
                # default `journalctl --priority=warning` queries.
                printf '%s\n' "$OUT" \
                        | systemd-cat -t shithub-aide -p warning
                exit 0
                ;;
        *)
                # Anything else: AIDE itself failed (missing DB, IO error,
                # config parse error). That's a different class — fail loud.
                {
                        echo "[$(ts)] aide RUN FAILED rc=$RC"
                        echo "$OUT"
                } >> "$LOG"
                printf 'aide run failed (rc=%s)\n%s\n' "$RC" "$OUT" \
                        | systemd-cat -t shithub-aide -p err
                exit "$RC"
                ;;
esac
