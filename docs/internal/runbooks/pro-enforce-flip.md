# Pro feature enforce-flag flip

Every personal-tier Pro feature ships with a per-feature
operator flag in `config.EnforceConfig` that gates whether the
deny path is **active** or **report-only**. PRO07 ratified this
as the one-way phased-rollout pattern; PRO-EXT01 every sprint
since adds more features under it.

This runbook documents the soak-then-flip sequence operators
follow when promoting a Pro feature from report-only to live
enforcement.

## When to use this

After a sprint that adds a new gated Pro feature merges and
deploys to production, the gate ships in report-only mode by
default. Reaching the deny path emits an
`entitlements.report_only_deny` log line with structured
fields:

- `feature` — the `Feature*` constant slug, e.g.
  `required_reviewers`, `profile_pins_beyond_free`.
- `principal_kind` + `principal_id` — who hit the gate.
- `reason` — `upgrade_required`, `billing_action_needed`, etc.
- `required_plan` — the plan the user would need.
- `mode` — `report_only`. (PRO-EXT01-02 added this field; older
  log entries from PRO07 do not carry it.)

The flag stays `false` until the operator has visibility into
the would-deny rate via these logs, then flips it via a
config change + redeploy.

> **Post-PRO-EXT01-17:** the campaign-wrap PR flipped every
> EnforceConfig default to `true` for the greenfield deployment
> case. Operators running with existing Free-user traffic that
> built up around a gated feature can selectively *re-disable*
> a flag by setting the matching TOML field to `false`; the
> guidance in this runbook then applies symmetrically for the
> revert direction.

## Soak window

- **Default: 7 days** of report-only logs in production.
- **Security-critical features: 14 days minimum** — anything
  in PRO-EXT01-11 (fine-grained PATs / IP allowlist) needs the
  longer window because a deny-path bug there is a credential
  escalation, not a UX papercut.

Per-feature soaks run independently. Flipping
`UserRequiredReviewers` to enforce does not affect
`UserAdvancedBranchProtection` or any other flag.

## Soak procedure

1. **Confirm deploy.** The feature must be live in the
   embedded binary and the operator config must have the new
   flag field. Check with:

   ```
   ssh root@shithub.sh "sudo -u shithub /opt/shithub/bin/shithubd config-dump | grep -i enforce"
   ```

   The expected default for every PRO-EXT01 enforce flag is
   `false`.

2. **Wait the soak window.** During the window, query the
   Prometheus counter shipped in PRO-EXT01-17:

   ```promql
   sum by (outcome) (rate(shithub_pro_gate_total{feature="<F>"}[1h]))
   ```

   Labels: `feature` (the `Feature*` slug), `kind` (`user` or
   `org`), `outcome` (`allow` or `deny`). What you're looking
   for: stable allow/deny ratio (no day-over-day climb) +
   absence of unexpected `kind` labels for kind-scoped features.

   The structured `entitlements.report_only_deny` log lines
   remain the per-event source of truth for spot-checks (the
   counter aggregates outcomes; the log carries `surface`,
   `principal_id`, `reason`).

3. **Review the report-only sample.** Look for:
   - Expected hits (real Free users trying the gated action).
   - Surprising hits (e.g. a Pro user denied — that's an
     entitlements bug, NOT a flip candidate).
   - High-volume noise (a single user repeatedly hitting the
     gate — suggests a confusing UI or an automated client).

   If anything looks wrong, **do not flip**. File the issue and
   fix in code first.

## Flip procedure

When the soak passes:

1. **Update operator config.** In the production Ansible
   inventory, set the per-feature flag to `true`. Example:

   ```yaml
   shithub_billing_enforce:
     user_required_reviewers: true     # PRO07 - flip
     user_advanced_branch_protection: false  # still soaking
     user_profile_pins_beyond_free: false    # still soaking
   ```

2. **Redeploy.** Use the standard deploy procedure — the new
   binary picks up the changed config at startup.

   ```
   make deploy ANSIBLE_INVENTORY=production
   ```

3. **Confirm the flip landed.**

   ```
   ssh root@shithub.sh "sudo -u shithub /opt/shithub/bin/shithubd config-dump | grep <flag>"
   ```

4. **Monitor for one hour.** Watch the error log for:
   - `entitlements.deny` — the new logged deny path (added by
     PRO-EXT01-17; until then, denies surface via the HTTP
     response, not a dedicated log line).
   - HTTP 402 + 4xx spikes on the relevant endpoint.
   - User support tickets about "I can't do X anymore."

   If something breaks badly, see the rollback section.

## Rollback

The enforce flag is technically reversible at the config level
— a redeploy with the flag back to `false` returns the gate
to report-only mode. **Do not treat this as routine, however.**
The PRO07 contract framed the flip as one-way; once a flag is
live, customers expect Pro to actually gate the feature.
Reverting it ships a "Pro feature became free again" signal.

Justification bar for reverting:

- **A real production bug** (e.g. Pro users being incorrectly
  denied) — yes, revert + fix.
- **Customer complaints about the gate itself** — no, the
  gate is the product. Adjust copy / messaging instead.

If reverting:

1. Set the flag back to `false` in Ansible.
2. Redeploy.
3. File the regression issue immediately and assign an owner.

## Per-feature soak snapshot (PRO-EXT01-17 wrap)

PRO-EXT01 left every flag at `false` (report-only). Operators
flip per the soak rules above. Per-feature notes:

| Feature key                      | Sprint  | Soak | Notes |
|----------------------------------|---------|------|-------|
| `user_profile_pins_beyond_free`  | 04      | 7d   | Cosmetic gate; low blast radius. |
| `user_profile_vanity`            | 04      | 7d   | Cosmetic. Enforce knob added in PRO-EXT_SR2-09 (audit caught it hard-enforcing with no soak path). |
| `animated_avatars`               | 04b     | 7d   | Free uploads flatten on flip. |
| `user_username_reservations`     | 05      | 7d   | Mostly Pro-only writes. Enforce knob added in PRO-EXT_SR2-09 (same fix as user_profile_vanity). |
| `private_repo_templates`         | 06      | 7d   | Toggle on settings/general. |
| `saved_replies_unlimited`        | 07a     | 7d   | Cap-enforce; soft visible. |
| `scheduled_issues`               | 07b     | 7d   | Worker-gated. |
| `advanced_code_search`           | 08      | 7d   | Search-quality gate. |
| `contribution_privacy`           | 09      | 7d   | Profile-visibility gate. |
| `secret_scan_history`            | 10      | 7d   | Worker short-circuits Free. |
| `secret_scan_alerts`             | 10d     | 7d   | Email + webhook delivery gate. |
| **`fine_grained_pats`**          | 11      | **14d** | Credential-escalation surface. |
| `user_actions_secrets`           | 12      | 7d   | Runner-side resolution. |
| `user_actions_variables`         | 12c     | 7d   | Runner-side resolution. |
| `webhook_relay`                  | 13a     | 7d   | Amplification surface. |
| `cron_workflow_dispatch`         | 13b     | 7d   | Worker-scheduled. |
| `personal_status_page`           | 14      | 7d   | Read-only page. |
| `repo_time_machine`              | 15      | 7d   | Per-repo write gate. |
| `inbox_rules`                    | 16a     | 7d   | Per-recipient rule eval. |
| `inbox_digests`                  | 16b     | 7d   | Worker-sweeps. |

When in doubt, the comment above the matching `EnforceConfig`
field in `internal/infra/config/config.go` is authoritative —
it carries any feature-specific landmines.

## Adding a new feature to this runbook

Each PRO-EXT01-NN sprint that adds a `Feature*` constant
should also add the corresponding `EnforceConfig` field. After
the sprint merges, add a one-line entry to the feature table
in `docs/internal/billing.md` so the soak status is visible at
a glance.

## See also

- `internal/infra/config/config.go` — `EnforceConfig` struct.
- `internal/entitlements/entitlements.go` — `Feature*`
  constants and `featureKinds`.
- `docs/internal/billing.md` — Pro tier feature matrix.
- `.docs/sprints/PAYMENTS/PRO-EXT01-README.md` — campaign
  overview (note: gitignored).
