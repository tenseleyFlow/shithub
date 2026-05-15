# PR checks

S24 ships the data shape, REST API, and PR Checks tab for "check
runs" — without executing user code. External CI systems
(Jenkins/Buildkite/etc., or future shithub Actions) post check runs
via PAT-authenticated `/api/v1/repos/{owner}/{repo}/check-runs`. The
required-check evaluator composes with the S23 review gate at
`pulls.Mergeability` and `pulls.Merge`.

## Schema (migration 0025)

```
check_suites — one row per (repo_id, head_sha, app_slug). Suites
               group runs by integration. Status + conclusion
               derived from runs (suite_rollup.go).
check_runs   — individual checks (e.g. "lint", "unit-tests").
               status: queued|in_progress|completed|pending
               conclusion: success|failure|neutral|cancelled|
                           skipped|timed_out|action_required|stale
```

Plus one column on `branch_protection_rules`:

| Column                                | Default | Notes                                              |
| ------------------------------------- | ------- | -------------------------------------------------- |
| `dismiss_stale_status_checks_on_push` | `false` | Opt-in: mark previous-head suites stale on push    |

`status_checks_required text[]` already existed from S20 as a
placeholder; this sprint activates enforcement.

The CHECK on `check_runs` enforces "completed implies conclusion".

## Suite is derived

API consumers POST runs; the orchestrator finds-or-creates the
matching suite via `(repo_id, head_sha, app_slug)`. Default
`app_slug = 'external'`. Future shithub Actions and external integrations
each get their own slug for grouping.

## Suite rollup

`internal/checks/suite_rollup.go::DeriveSuiteRollup` is the pure
function:

```
status = 'completed'   iff every run is completed
       = 'in_progress' if any run has moved past 'queued'
       = 'queued'      otherwise

conclusion priority (when status='completed'):
  failure > timed_out > cancelled > action_required >
  success > neutral > skipped > stale
```

Re-evaluated inside the same tx as Create/Update so the API response
reflects the latest state.

## Merge gate composition

The S22 mergeable_state machine is now (priority order):

```
dirty   — git merge-tree reports conflicts
behind  — head has no commits ahead of base
blocked — review gate (S23) OR checks gate (S24) unsatisfied
clean   — both gates satisfied
```

`pulls.Mergeability` evaluates both gates and ANDs their satisfaction.
`pulls.Merge`'s belt-and-braces re-evaluation inside the row lock
also runs both. Either failing produces `ErrMergeBlocked`.

The required-check evaluator is in
`internal/checks/required_eval.go::EvaluateRequiredChecks`. A run
"satisfies" iff `status='completed'` AND
`conclusion ∈ {success, neutral}` on the PR's head SHA.

## API surface

| Method | Path                                                            | Scope        |
| ------ | --------------------------------------------------------------- | ------------ |
| POST   | `/api/v1/repos/{owner}/{repo}/check-runs`                       | `repo:write` |
| PATCH  | `/api/v1/repos/{owner}/{repo}/check-runs/{id}`                  | `repo:write` |
| GET    | `/api/v1/repos/{owner}/{repo}/commits/{sha}/check-runs`         | `repo:read`  |
| GET    | `/api/v1/repos/{owner}/{repo}/commits/{sha}/check-suites`       | `repo:read`  |

Visibility is also gated by `policy.Can` (existence-leak 404 for
private repos the actor can't see).

### POST request body

```json
{
  "name": "lint",
  "head_sha": "<40-char SHA>",
  "app_slug": "external",
  "status": "in_progress",
  "started_at": "2026-05-08T12:00:00Z",
  "details_url": "https://ci.example.com/job/123",
  "output": {
    "title": "lint",
    "summary": "5 warnings",
    "text": "...full log under 256 KiB..."
  },
  "external_id": "ci-job-42"
}
```

`external_id` makes retries safe: when set, repeating the same POST
returns the existing run instead of inserting a duplicate.

shithub Actions creates check runs with a local `details_url` pointing
at the workflow run page (`/{owner}/{repo}/actions/runs/{run_index}`).
Code-tab status indicators use that safe local URL when available; API
clients may still provide external CI URLs through the public check-run
API, but repo Code surfaces intentionally do not render external
redirect targets as status links.

### PATCH request body

Every field is optional. Omitted fields keep their current value.
`status='completed'` requires a `conclusion`. Re-applying the same
status is a no-op.

### Response shape

Mirrors GitHub's check-run shape: `id, suite_id, head_sha, name,
status, conclusion, started_at, completed_at, details_url, output,
external_id`. RFC3339 timestamps. Empty `conclusion` omitted.

## Output sizes

Per spec: `output.summary` capped at 64 KiB; `output.text` capped at
256 KiB. The Create/Update orchestrator returns
`ErrOutputSummaryTooLarge` / `ErrOutputTextTooLarge` (HTTP 400) on
overflow.

## PR Checks tab

`internal/web/handlers/repo/pulls.go::pullChecks` loads suites for
the PR's `head_oid`, then loads runs grouped under each suite. The
`output.summary` field renders through the existing markdown
pipeline (Goldmark + bluemonday UGCPolicy). When no checks have
reported on the head SHA, an empty-state message points at the
POST endpoint.

## Stale-on-push

Opt-in via `branch_protection_rules.dismiss_stale_status_checks_on_push`
on the longest-pattern-matching rule. When `true`, `push:process`
calls `checks.MarkStaleForPreviousHead(repo, before_sha)` after a
head ref moves. Suites with `status != 'completed'` get
`(status='completed', conclusion='stale')`. The runs themselves
stay readable for audit.

The default is `false` to match GitHub. Most CI integrations
already handle "this push obsoletes my in-flight build" via their
own retry semantics.

## Branch-protection settings UI

`/{owner}/{repo}/settings/branches` adds two fields:

- **Required status checks** — comma-separated check-run names that
  must succeed on the head SHA before merge. Empty list means no
  requirement.
- **Mark in-flight checks stale on new push** — checkbox; sets
  `dismiss_stale_status_checks_on_push`.

The protection-rule table now shows the `checks: [name1, name2]`
list inline so admins can verify their config at a glance.

## Tests

| File                                              | Covers                                                 |
| ------------------------------------------------- | ------------------------------------------------------ |
| `internal/checks/suite_rollup_test.go`            | Pure rollup priority order across 12 cases             |
| `internal/checks/checks_test.go::AutoCreatesSuite` | First run on a (repo, sha, app) creates the suite      |
| `…::IdempotentByExternalID`                       | Same external_id POST returns same run id              |
| `…::RequiresConclusionWhenCompleted`              | `status=completed` without conclusion → 400            |
| `…::RejectsShortHeadSHA`                          | Head SHA must be ≥ 7 chars                             |
| `…::RejectsTooLargeOutput`                        | Output text > 256 KiB → 400                            |
| `…::RollsUpSuiteConclusion`                       | Two completed-success runs → suite success            |
| `…::EvaluateRequiredChecks_NoRequired`            | Empty required-list → satisfied                        |
| `…::BlocksThenSatisfies`                          | None → queued → failure → success state machine        |
| `…::StaleOnPush_MarksSuites`                      | Previous-head suite goes (completed, stale)            |
| `…::TimestampsRoundTrip`                          | started_at + completed_at round-trip via PATCH         |

## Pitfalls handled

- **`merge-tree --merge-base=<base>` semantics** — fixed in S22; not
  re-introduced here.
- **Required-check name typo** — surfaced inline in the protection-
  rule table so admins notice. Cross-checking against "names seen on
  default branch in last 30d" is a follow-up (see Deferred).
- **External-system retries** — `external_id` dedupes Create; PATCH
  is idempotent (re-applying the same status is a no-op via the
  status comparison).
- **Race between check post and head push** — checks key on
  `head_sha`; a check posted for an old SHA correctly does not
  satisfy the new SHA's required-check rule.
- **Output payload bombs** — capped at 256 KiB / 64 KiB at the
  orchestrator before insert.
- **TOCTOU on merge** — the row-lock pre-flight in `pulls.Merge`
  re-evaluates both gates so a check failing between Mergeability
  and Merge is honored.

## Deferred

- **Workflow definition / `.shithub/workflows/*.yaml` parsing** →
  Actions sprint (post-MVP).
- **Runner protocol / runner pool** → Actions sprint (post-MVP).
- **Check log storage / live log streaming** → post-MVP.
- **GitHub Statuses API parity** → post-MVP. Spec lean: defer.
- **Required-check name typo warning** ("name not seen in last 30d
  on default branch") → S36 (perf pass groups UI nudges there).
- **Webhook events for `check_suite` / `check_run`** → S33 (the
  deliverer drains `webhook_events_pending`; emit points are at
  `internal/checks/{create,update}.go`).
- **CODEOWNERS-driven required reviewers + checks** → post-MVP.
- **`require_code_owner_review` enforcement** → post-MVP (column
  exists from S23; CODEOWNERS parser is the gap).
- **Auto-dismiss stale reviews on push** → post-MVP (column exists
  from S23; symmetric to S24's stale-on-push, but the reviewer-side
  semantics are spec-deferred).
