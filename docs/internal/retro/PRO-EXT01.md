# PRO-EXT01 retro — personal Pro tier expansion

**Status:** complete. 17 sprints, ~20 PRs landed to trunk; campaign
wrapped in PR-17.

## Scope at start of campaign

Personal Pro shipped one live feature (`FeatureProfilePins`, 6→100).
Two other gates (`FeatureRequiredReviewers`,
`FeatureAdvancedBranchProtection`) were deployed in report-only mode.
The compare-plans page didn't mention the pins jump; there was no
`/api/v1/user/plan` for CLIs; no Pro badge anywhere; no consistent
"locked-UI" affordance.

## What we shipped

### Phase 1 — Cleanup (sprints 01–03)
- `pro-lock` template macro + Pro badge component (01)
- Enforce-flag flip plumbing + visible pin cap (02)
- `/api/v1/user/plan` + Stripe edge-case hardening (03)
- Operator runbook for the report-only → enforce flip cadence

### Phase 2 — Feature sprints (04–16)
- **Bucket A (identity):** profile vanity, animated GIFs, vanity
  username reservations, Pro pill sweep on issue/PR views.
- **Bucket B (productivity):** private repo templates, saved replies
  (unlimited + scheduled), cross-repo regex code search with saved
  queries.
- **Bucket C (privacy/security):** contribution-graph privacy,
  personal secret scanning + alerts (email + HMAC-signed webhook),
  fine-grained PATs (IP allowlist, single-repo binding, usage
  analytics).
- **Bucket D (automation):** personal Actions secrets + variables,
  webhook relay, cron workflow dispatch, personal status page +
  uptime SVG badges, repo time-machine ("paused"), focus-mode inbox
  with rules + digests.

### Phase 3 — Wrap (sprint 17)
- `shithub_pro_gate_total{feature,kind,outcome}` Prometheus counter,
  auto-instrumented via an entitlements observation hook.
- Runbook updated with per-feature soak snapshot.
- This retro.

### Stabilisation rounds (PRO-EXT_SR-01 through 08)
Mid-campaign we paused for a remediation round: blocking bound PATs
from `/api/v1/user/actions/*`, wiring `report_only_deny` telemetry on
runner + API gates, fixing a migration constraint break, adding
positive Pro+enforce tests for the runner path, REST-PUT enforce
tests, scope migration to `user:read/user:write`, production HTML
render tests for settings pages, and a batch-N+1 cleanup in the
user-secrets merge.

## Reusable patterns that paid off
- **Phased enforce flags** (PRO07 — one bool per feature in
  `EnforceConfig`, default `false`). Every new gate dropped into
  this with no new decision logic.
- **Report-only soak with structured logs.** The
  `entitlements.report_only_deny` shape (`feature`, `surface`,
  `mode`, `principal_*`) survived 20 PRs without a single
  destructive schema change. PRO-EXT01-17's Prometheus counter
  bolts onto the same call site.
- **Locked-UI tooltips backed by the same entitlement check.**
  Every Pro feature has a Free-side affordance — disabled control +
  upgrade CTA — that consults the same `CheckPrincipalFeature` call
  the write path runs. Discovery is the trial.
- **Kind-aware registry** (`featureKinds`). New `Feature*` constants
  must register or `KnownFeature` rejects them — caught two missed
  registrations during code review across the campaign.

## What surprised us
- **Production template render tests** (PRO-EXT_SR-07) caught more
  bugs than the per-handler `httptest` tests. Catching a missing
  `disabled` attribute in HTML is much easier in a string-match
  test than in a Selenium-style flow.
- **The runner credential surface** (sprints 12/12b/12c) needed
  two follow-ups (vars 12c, telemetry SR-02). The original 12
  sprint underestimated how many call sites needed the gate.
- **Stripe webhook idempotency** (sprint 03) saved several hours
  of debugging during 06pre's test failures.
- **PR slicing matters.** The three webhookrelay/cron/automation
  PRs (13a/13b/13c) merged cleanly because each stayed under
  ~600 lines and shipped a self-contained vertical slice.
  Sprint 10 (secret scanning) bundled too much into one PR
  initially — we split it into 10a/10b/10c/10d for review-ability.

## What we left for follow-up
- **Operator playbook for the actual flips.** This campaign shipped
  the *machinery* for the report-only → enforce flip. In the
  follow-up commit that ships with sprint 17 we flipped every
  EnforceConfig default to `true` after confirming the deployment
  has no built-up Free-user reliance on any gated feature. Operators
  running a deployment with existing Free traffic can selectively
  disable enforcement per-feature in TOML (BurntSushi/toml only
  overwrites named fields). The runbook
  (`docs/internal/runbooks/pro-enforce-flip.md`) describes the
  observation cadence; for greenfield it's not load-bearing.
- **Webhook relay retry** (sprint 13a): the delivery worker retries
  in-band; harden into a separate retry queue with backoff during a
  future SR round.
- **Pro pill on additional surfaces.** PRO-EXT01-04c added pills to
  issue/PR view authors + participants. The explore/people page,
  star/follower lists, organization member lists, and commit author
  surfaces remain untreated.
- **Animated avatars APNG support.** PRO-EXT01-04b preserves GIF
  bytes; APNG support would need a separate sniff path + a
  Content-Type decision matrix.

## Operational expectations after the campaign
- Personal Pro now sells ~20 distinct value items vs. the original
  one (pin cap). The compare-plans table reflects each.
- Every gate emits Prometheus counters + structured logs; SRE
  dashboards have one source-of-truth metric (`shithub_pro_gate_total`).
- New Pro features extend this campaign's pattern: add a `Feature*`
  constant, register the kind, add an `EnforceConfig` field, mirror
  one of the existing handler-side gates, add a template-level
  Pro-lock variant, add a production HTML render test, and a
  positive-and-negative entitlement test. The whole cycle is ~1 day
  of work for a feature with no novel storage requirements.

## Numbers
- 17 sprints, 20 trunk PRs (plus 8 stabilisation PRs and 4 deferred
  follow-up PRs).
- 18 new `Feature*` constants across `internal/entitlements`.
- 16 new `EnforceConfig.*` fields.
- 1 new Prometheus counter.
- 20 enforce flags flipped to enforce-by-default in the wrap PR
  (greenfield deployment; no prior Free-user reliance to migrate).
