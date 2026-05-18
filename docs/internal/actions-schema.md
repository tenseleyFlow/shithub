# Actions/CI — schema + workflow dialect (S41a)

The Actions/CI subsystem is shipping in eight sub-sprints (S41a through
S41h, plus optional S41i Nix engine). This doc covers what S41a lays
down: the SQL schema, the workflow YAML dialect, the expression
evaluator, and the load-bearing taint contract every later sub-sprint
depends on.

S41a is parser + schema only — no triggers, no runner, no UI. The
goal is to land a frozen contract that S41b/c/d/e can build against
without churning under them.

## SQL schema

Actions migrations currently span 0042–0051, 0053, 0057, 0060, 0064–0067,
0080, and 0109–0110.
Migration 0052 belongs to the repo source-remotes feature, 0054
belongs to push event protocol tracking, 0055 belongs to the social
feed, 0056 belongs to user profile contribution settings, 0058 belongs
to repo name reuse, and 0059 belongs to GitHub org imports.

| #     | Table                       | Purpose                                                       |
| ----- | --------------------------- | ------------------------------------------------------------- |
| 0042  | `workflow_runs`             | One row per triggered workflow execution                      |
| 0043  | `workflow_jobs`             | Jobs within a run (one row per `jobs.<key>`)                  |
| 0044  | `workflow_steps`            | Steps within a job (one row per `steps[i]`)                   |
| 0045  | `workflow_secrets`          | Per-repo + per-org encrypted secrets                          |
| 0046  | `workflow_runners`          | Registered runners + `runner_tokens`                          |
| 0047  | `workflow_step_log_chunks`  | Hot-path append log buffer (concatenated to blob on finalize) |
| 0048  | `workflow_artifacts`        | Per-run artifact metadata (90-day default expiry)             |
| 0049  | `actions_variables`         | Non-secret per-repo/org config (Forgejo parity)               |
| 0050  | `workflow_steps.step_with`  | Parsed `with:` inputs for magic `uses:` aliases               |
| 0051  | `workflow_runs.trigger_event_id` | Trigger idempotency for retries/admin replays            |
| 0053  | `runner_jwt_used`           | Single-use replay gate for runner job JWTs                    |
| 0057  | `workflow_job_secret_masks` | Encrypted claim-time log mask snapshots per job               |
| 0060  | Actions retention indexes   | Narrow cleanup indexes for terminal steps/runs                |
| 0066  | `actions_*_policies`, `workflow_run_approvals` | Enablement, runner-pool caps, and approval decisions |
| 0067  | `workflow_runners` ops state | Host/version metadata, drain state, and hard revocation state |
| 0080  | `workflow_annotations`      | Sanitized workflow-command notice/warning/error annotations   |
| 0109  | `repo_environments`         | Repo Actions environments, deployment branch policy, and environment-scoped secrets |
| 0110  | `shithub_deployment_pattern_matches` | Runner-claim wildcard matcher for environment deployment policies |

A few load-bearing choices, called out so they're easy to spot in a
later schema diff:

- **`workflow_runs.run_index`** — per-repo monotonic counter. Each
  repo gets `#1`, `#2`, … so URLs like
  `/{owner}/{repo}/actions/runs/42` are stable and human-friendly.
  Crib from Forgejo's `actions_run.index`.
- **`workflow_runs.version`** — optimistic-lock counter. Mutators
  bump-and-check rather than `SELECT … FOR UPDATE`. Required for
  S41g's race between a cancel request and a state transition.
- **`workflow_runs.concurrency_group`** — the concurrency-slot key,
  resolved at trigger time from the workflow's `concurrency.group:`
  expression. S41g's slot manager keys off this column and runner
  claim blocks younger runs while an older same-group run still has a
  queued/running job without `cancel_requested=true`.
- **`workflow_runs.parent_run_id`** — for re-runs. The new run
  references the original; the UI shows a "re-ran from #N" link.
- **`workflow_jobs.runner_id`** — FK added in 0046 (after the
  runners table exists). Nullable until claimed.
- **`workflow_jobs.environment_name` / `environment_url`** — raw
  parsed `jobs.<key>.environment` data. These are text columns rather
  than FKs because GitHub permits workflow-authored environment names;
  the matching `repo_environments` row exists only when an owner has
  configured protection, deployment branch policy, or environment
  secrets for that name.
- **`workflow_steps`** has a CHECK constraint enforcing
  `(run_command IS NOT NULL) <> (uses_alias IS NOT NULL)` — exactly
  one of `run:` or `uses:`. The `uses_alias` column is further
  CHECK-constrained to the three magic aliases we accept in v1.
- **`workflow_secrets`** owns its value as `bytea` ChaCha20Poly1305-
  sealed via `internal/auth/secretbox`. Key derivation uses
  `cfg.Auth.TOTPKeyB64` (already an operator-managed root) +
  `(owner, kind, name)` salt so re-keying is per-row.
- **`repo_environments`** stores the configured repo environment row
  plus required-reviewer flags, wait timer, and deployment branch
  policy. Runner claim enforces selected-branch and protected-branch
  deployment policies, wait timers, and required-reviewer holds before
  a job can leave `queued`. Required-reviewer approval UI is still a
  follow-up SP23 surface; until then, a manually enabled reviewer gate
  intentionally keeps matching deployment jobs queued.
- **`workflow_step_log_chunks.chunk`** is capped at 512 KB per row.
  The runner sends bigger payloads in pieces. `(step_id, seq)` is
  UNIQUE so duplicate sends are idempotent.
- **`actions_variables`** — non-secret, plaintext, scoped exactly
  like secrets (per-repo or per-org, never both on the same row).
  Forgejo has the same split; we mirror it for parity.
- **`runner_jwt_used`** — primary-keyed by JWT `jti`. Job endpoints
  insert into this table during auth; zero inserted rows means replay
  and the API returns 401. JWTs are HMAC-SHA256 and use an HKDF
  subkey derived from `auth.totp_key_b64` with label
  `actions-runner-jwt-v1`.
- **`workflow_job_secret_masks`** — one encrypted JSON array of exact
  secret values per claimed job. It snapshots the log scrub set at
  claim time, preventing a rotated or deleted secret from disappearing
  from server-side masking while the old value is still in a runner's
  job payload.
- **`workflow_annotations`** — notice/warning/error records parsed from
  complete workflow-command log lines after server-side log scrubbing.
  Rows are tied to run/job/step, store capped sanitized text plus
  optional source location metadata, and use a step-scoped fingerprint
  so runner retries are idempotent.
- **`actions_site_policy`, `actions_org_policies`,
  `actions_repo_policies`** — inherited Actions enablement and abuse
  caps. Runner claim and trigger enqueue both read the effective policy:
  repo override, then org override, then site default.
- **`workflow_run_approvals`** — one approval-decision row for every run
  whose `workflow_runs.need_approval` flag is set. Approval records the
  maintainer and lets runner heartbeats claim the existing queued jobs;
  rejection completes the run with `action_required`.

The `version` and `run_index` patterns are the two pieces I'd point
out to a future maintainer first. Both are cheap to add now and
miserable to retrofit later.

## Workflow YAML dialect (v1)

We accept a strict subset of GitHub Actions YAML. The parser rejects
unknown keys at parse time so workflow authors find their typos
immediately instead of shipping a workflow that does nothing.

### Top level

```yaml
name: my-pipeline                         # optional human name
on: [push, pull_request]                  # or full-form (see below)
permissions: read-all                     # default if omitted
env: { GREETING: "hello" }                # workflow-level env
concurrency:                              # optional slot manager
  group: ${{ shithub.ref }}
  cancel-in-progress: true
jobs:
  <key>:                                  # 1+ entries
    runs-on: ubuntu-latest
    needs: [other-key]                    # optional dep edge
    if: ${{ shithub.actor == 'alice' }}   # optional gate
    timeout-minutes: 60                   # 1..4320, default 360
    permissions: { contents: read }       # narrow workflow perms
    environment: production               # or { name: ..., url: ... }
    env: { K: v }                         # job overlay
    steps:
      - name: ...
        id: ...
        if: ...
        run: echo hi                      # run XOR uses
        uses: actions/checkout@v4         # exactly one of three aliases
        working-directory: ...
        env: { ... }
        continue-on-error: false
```

### Triggers (`on:`)

v1 supports four triggers — anything else is a parse error.

| Trigger             | Surface                                                          |
| ------------------- | ---------------------------------------------------------------- |
| `push`              | `branches:`, `tags:`, `paths:` (include + `!exclude` semantics)  |
| `pull_request`      | `types:` (opened/synchronize/reopened/...), `branches:`, `paths:` |
| `schedule`          | one or more `- cron: <5-field-expr>`                             |
| `workflow_dispatch` | `inputs:` map (string/boolean/choice/environment)                |

### `uses:` allowlist

Exactly three aliases are reserved at parse time, no exceptions:

| Alias                            | Parser status | Runner status                              |
| -------------------------------- | ------------- | ------------------------------------------ |
| `actions/checkout@v4`            | accepted      | executable with scoped checkout token      |
| `shithub/upload-artifact@v1`     | accepted      | rejected until artifact upload lands       |
| `shithub/download-artifact@v1`   | accepted      | rejected until artifact download lands     |

Any other `uses:` value (community actions, Docker images, composite
actions) is an Error-severity diagnostic. The marketplace problem is
explicitly out of scope for v1; revisit only if a real demand exists
and we have an answer for supply-chain trust.

The current Docker executor runs `actions/checkout@v4` and `run:` steps.
Checkout happens on the runner host before a containerized step mounts the
workspace. The server issues a short-lived checkout-purpose JWT scoped to
the claimed repository and running job; the smart-HTTP handler accepts it
only for read-only `git-upload-pack`. Artifact transfer remains explicit
follow-up work, and the artifact aliases fail deliberately until that path
exists.

Checkout v1 accepts only `with.fetch-depth`. The default is a depth-1 fetch
of the workflow run's `head_sha`; `fetch-depth: 0` requests full history.
Submodules, LFS, `path`, persisted credentials, and marketplace actions are
rejected because they are not part of this dialect yet.

### File-size + parser caps

- **64 KB** workflow file size cap (`workflow.MaxWorkflowFileBytes`).
  Files larger than this are rejected before YAML decode begins —
  defends against pathological inputs and gives operators a
  predictable upper bound on parser memory.
- **100 anchors** per document (`workflow.MaxYAMLAliases`) — the
  billion-laughs guard. yaml.v3 doesn't expose a direct knob; we
  count alias nodes during a tree walk and bail.

### `${{ github.* }}` alias

The dialect is intentionally rebranded to `${{ shithub.* }}`.
Authors who paste GHA workflows in unmodified will see their
`${{ github.* }}` references continue to work because the evaluator
rewrites `path[0]` from `github` to `shithub` at the top of `evalRef`
before taint computation, dispatch, and error rendering.

The alias is intentionally **scope-narrow**: only fields that exist
in our `shithub.*` namespace (`run_id`, `sha`, `ref`, `actor`,
`event`) route through. GHA fields we don't expose in v1 —
`event_name`, `repository`, `run_number`, `workspace`, etc. — error
with the canonical `unknown shithub field "X"` message. Slightly
confusing for a GHA-flavored author but keeps the v1 namespace
surface tight.

The alias preserves the load-bearing taint flag: `github.event.X`
taints exactly like `shithub.event.X`. `TestEval_GithubAliasIsTainted`
pins this contract.

Migration to strict-compat (drop the alias entirely) later is a
one-PR flip; moving the other direction is much harder.

This is a deliberate decision recorded in the campaign plan.

## Expression evaluator

`${{ … }}` expressions are parsed into a tiny AST and evaluated by
`internal/actions/expr`. The surface is intentionally minimal:

### Allowed namespaces

| Namespace        | Source            | Tainted?                    |
| ---------------- | ----------------- | --------------------------- |
| `secrets.X`      | workflow_secrets  | no, but sensitive           |
| `vars.X`         | actions_variables | no (operator-controlled)    |
| `env.X`          | workflow file     | no (workflow author's text) |
| `shithub.run_id` | dispatch context  | no                          |
| `shithub.sha`    | dispatch context  | no                          |
| `shithub.ref`    | dispatch context  | no                          |
| `shithub.actor`  | dispatch context  | no (resolved username)      |
| `shithub.event.*`| trigger payload   | **yes — always**            |

`runner.*`, `steps.*`, `needs.*`, `matrix.*`, `inputs.*` are all
parse-time errors. They're parked for v2 and the parser's
allowlist-closed posture means a future PR can't widen this
accidentally without a clearly visible diff.

### Allowed functions

`contains(haystack, needle)`, `startsWith(s, prefix)`,
`endsWith(s, suffix)`, plus the four job-status predicates
`success()`, `failure()`, `cancelled()`, `always()`. That's the
whole list. `fromJSON`, `hashFiles`, `toJSON`, `format`, and
friends are explicitly rejected — they each carry footgun risk
(parser DoS, FS access, side-channel injection) that we don't want
to take on in v1.

### Missing-value semantics

| Reference                        | Missing → ?                          |
| -------------------------------- | ------------------------------------ |
| `secrets.NOT_BOUND`              | error (loud — workflow won't run)    |
| `vars.MISSING`                   | empty string (GHA parity)            |
| `env.MISSING`                    | empty string (GHA parity)            |
| `shithub.event.deeply.missing`   | null **but still tainted**           |

The "missing event path → null but tainted" case is a defence-in-
depth choice: even if the path doesn't resolve, the result still
came from the event payload, and we'd rather over-flag than under.

## Taint contract — the load-bearing piece

This is the contract every later sub-sprint hangs off. Get it wrong
and we have an injection-shaped hole in the runner.

### Where the flag lives

The taint flag lives on `expr.Value` (the evaluator-produced value),
not `workflow.Value` (the parser-produced value). Two different
structs share the name `Value` because they live in different
packages, but they have different jobs:

- **`workflow.Value`** carries the raw source string the parser read
  out of the YAML (an env entry, a `with:` input, a concurrency
  group expression). At parse time we don't know what the
  `${{ … }}` body will resolve to, so there's nothing to taint yet.
- **`expr.Value`** is what the evaluator returns when it resolves a
  reference at runtime. *This* struct carries `Tainted bool`. The
  runner's exec layer (S41d) consumes that flag.

Pre-L5 the parser-side struct also had a `Tainted bool` field plus a
`Tainted()` constructor — both unused, both confusing because they
suggested two sources of truth. Dropped in S41a-L5 cleanup.

### Propagation

**Every `expr.Value` carries a `Tainted bool`.** Set true iff the
value transitively depends on `shithub.event.*`. Operators control
secrets, vars, env, the rest of `shithub.*`. Authors control the
workflow file. Only the event payload is *attacker-controlled*: a
PR title, a commit message, a branch name from a fork. Those values
must never be interpolated into a shell string.

Propagation rules:

- Reading `shithub.event.X` → `Tainted: true` (always, including
  missing-path null results).
- Reading `secrets.X` → `Sensitive: true`. Secrets are operator-
  controlled, so they are not tainted, but they must not appear in
  shell source strings or Docker argv.
- Reading any other namespace → `Tainted: false` and
  `Sensitive: false`, except `env.X` preserves both flags of the
  resolved env value. This closes the escape where an event-derived or
  secret-derived value is first assigned to env and then interpolated
  through `${{ env.X }}`.
- Binary op (`==`, `!=`, `&&`, `||`) → tainted or sensitive if either
  operand is.
- Unary op (`!`) → tainted/sensitive iff its operand is.
- Function call (`contains`, `startsWith`, `endsWith`) → tainted or
  sensitive if any argument is.

The runner consumes `Tainted` and `Sensitive` and refuses to interpolate
either class into shell strings. Instead, those values are bound to
runner-owned `SHITHUB_INPUT_xx` envvars and the shell source only
references those placeholders. The author writes:

```yaml
- run: echo "PR title was: ${{ shithub.event.pull_request.title }}"
```

The runner sees a tainted reference; it compiles the step to:

```bash
SHITHUB_INPUT_0="$user_pr_title" exec sh -c 'echo "PR title was: $SHITHUB_INPUT_0"'
```

…where `$user_pr_title` is set via Go's `cmd.Env`, never inserted into
the shell source string or Docker CLI argv. Backticks, `$()`, `;`,
`&&` — none of those work as command-injection vectors when the value
reaches the shell as environment data instead of syntax.

The shared renderer lives in `internal/runner/exec`, so future engines
consume the same injection boundary instead of reimplementing it. The
runner claim payload includes `workflow_runs.event_payload`; without
that field, the runner cannot evaluate and taint
`${{ shithub.event.* }}` references.

Tests for this contract live in `internal/actions/expr/eval_test.go`,
`internal/runner/exec/render_test.go`, and
`internal/runner/engine/docker_test.go`. **Do not** weaken them in a
later PR without an audit-checkpoint review — they're explicitly
load-bearing for S41e's threat model.

Runner log chunks pass through `internal/runner/scrub` before they are
posted to the API. It masks exact secret values and preserves enough
tail bytes between chunks to catch a secret split across chunk
boundaries. S41e wires resolved workflow secrets into the runner claim
payload and mask set, snapshots that mask set encrypted on the job, then
applies the same exact-value scrub again in the runner API before
persisting chunks. The server path also carries a possible secret-prefix
tail from the prior persisted chunk, so a runner that bypasses
client-side scrubbing cannot leak a secret by splitting it across
adjacent log POSTs.

## `shithub.event` payload schema (v1)

The event payload is the most user-facing part of the contract: once
authors write workflows that template against `shithub.event.X`,
schema changes are breaking. The v1 schema is pinned and labelled
`v1`. Any addition is fine; renames and removals require a major
bump.

The schema is enforced by **typed constructors** in the
`internal/actions/event` package — one per trigger. S41b's pipeline
calls these to build payloads; the function signatures pin the
field set so adding a key requires editing the constructor in a
visible diff. This is the same closed-door discipline as the
expression evaluator's namespace allowlist.

| Trigger             | Constructor             | Top-level keys                                                                    |
| ------------------- | ----------------------- | --------------------------------------------------------------------------------- |
| `push`              | `event.Push`            | `ref`, `before`, `after`, `head_commit{message,id,author}`                        |
| `pull_request`      | `event.PullRequest`     | `action`, `number`, `pull_request{title,head{ref,sha},base{ref,sha},user{login}}` |
| `schedule`          | `event.Schedule`        | (empty map — cron fired; cron expression is on the `workflow_runs` row)           |
| `workflow_dispatch` | `event.WorkflowDispatch`| `inputs{<name>: <stringified>}`                                                   |

Anything not in this table doesn't exist in v1. Accessing it returns
null+tainted (the missing-path semantics above).

**Adding a field**: edit the constructor in `internal/actions/event/`,
add a row to this doc, and update the corresponding `*_FlowsThroughEvaluator`
test in `event_test.go` so the new path is exercised end-to-end.
Reviewer-required note in the commit message — same standard as a
new evaluator function.

**Renaming or removing**: that's a v1→v2 break. Don't.

## Operator surface

`shithubd admin actions parse <file>` reads a workflow off disk,
runs the parser, and dumps diagnostics + a canonical JSON rendering
of the parsed AST. Useful for:

- debugging "why is my workflow not picking up changes" reports
- validating a workflow file before committing it
- producing a stable AST snapshot for inclusion in bug reports

Exit codes:

| Code | Meaning                                       |
| ---- | --------------------------------------------- |
| 0    | clean parse, no Error-severity diagnostics    |
| 1    | file unreadable, oversized, or YAML malformed |
| 2    | parse produced Error-severity diagnostics     |

Other admin surfaces are scoped to later sub-sprints:

- S41c: `shithubd admin runner register --name <foo>` issues a
  registration token + writes a row to `workflow_runners`.
- S41j: `shithubd admin runner drain|undrain|rotate-token|revoke|cleanup-stale`
  gives operators pool controls. Drained runners keep heartbeating and
  may finish already claimed jobs but receive no new claims. Revoked
  runners are set offline, all registration tokens are revoked, and job
  API JWTs from that runner are rejected even if the runner still has an
  old config file.
- S41g: `POST /api/v1/jobs/{id}/cancel` and the repository run-detail
  UI request cancellation. Running jobs flip `cancel_requested`; queued
  jobs are made terminal immediately.
- S41g: `POST /api/v1/runs/{id}/rerun` and the repository run-detail
  UI re-run completed/cancelled runs. Re-runs read the workflow YAML
  from the original run's `head_sha`, create a fresh queued
  `workflow_runs` row, and set `parent_run_id` to the source run.
- S41g: workflow-level `concurrency.group` is resolved at enqueue time
  against the trigger context (`shithub.ref`, `shithub.sha`, and
  `shithub.event.*`). With `cancel-in-progress: true`, enqueue requests
  cancellation for older active runs in the same group. Without it,
  runner claim leaves the younger run queued until the older run no
  longer has uncancelled queued/running jobs.
- S41g: `workflow:cleanup` is a daily retention worker enqueued by
  `shithubd-cron.service`. Operators can run it manually with
  `shithubd admin run-job workflow:cleanup`.

## Workflow concurrency (S41g)

`concurrency.group` is a workflow-level slot key. The parser stores the
raw value, and `internal/actions/concurrency` evaluates `${{ ... }}`
fragments when the run is enqueued. The trigger-time context deliberately
does not include secrets; event-derived values may be tainted but are
safe here because the value is only used as a database key.

When a run enters a non-empty group:

- `cancel-in-progress: false` leaves the new run queued behind older
  same-repo, same-group runs while those older runs still have
  queued/running jobs with `cancel_requested=false`.
- `cancel-in-progress: true` requests cancellation on those older jobs.
  Queued jobs become terminal immediately; running jobs keep running
  with `cancel_requested=true` so the runner can kill the active
  container. Once every active older job is cancel-requested, the group
  is released for the newer run.

The runner claim query enforces the queueing rule, not the web handler
or UI. This keeps heartbeat races honest: multiple runners can poll at
the same time, but only jobs whose dependency and concurrency blockers
are clear can be claimed.

## Runner timeouts (S41g)

`jobs.<key>.timeout-minutes` is enforced by `shithubd-runner` as a
whole-job deadline. The parser stores the value in
`workflow_jobs.timeout_minutes` with the GitHub-compatible default of
360 minutes and a 1..4320 cap.

When the deadline expires, the Docker engine explicitly kills the
active step container, emits a terminal step update with
`status=completed` and `conclusion=timed_out`, and the runner reports
the job itself as `completed/timed_out`. The server rolls the parent
workflow run up to `timed_out` when all jobs are terminal. A timed-out
step is not masked by `continue-on-error`; the job deadline always wins.

The runner API increments `shithub_actions_step_timeouts_total` the
first time a step reaches `conclusion=timed_out`. Duplicate terminal
step-status retries do not increment the counter again.

## Retention cleanup (S41g)

`workflow:cleanup` applies the durable Actions retention contract in
this order:

1. Delete hot `workflow_step_log_chunks` for steps completed more than
   7 days ago. Finalized logs already live in object storage.
2. Delete expired `workflow_artifacts` rows after deleting their
   `actions/runs/...` blob objects. The row's `expires_at` value is
   authoritative so per-upload retention overrides keep working.
3. Delete unpinned terminal `workflow_runs` older than 365 days. Child
   jobs, steps, artifacts, and consumed JWT rows cascade through FK
   ownership.
4. Delete consumed `runner_jwt_used` rows whose JWT expiry is more than
   30 days old. This preserves replay/audit evidence for recent jobs
   without letting the replay table grow forever.

The defaults can be overridden in the worker payload:

```json
{"step_log_chunk_days":7,"run_days":365,"jwt_used_days":30,"artifact_batch":1000}
```

`artifact_batch` caps each object-delete page and may not exceed 10000.
Negative values are poison-job errors. The worker exports
`shithub_actions_runs_pruned_total{kind}` where `kind` is one of
`chunks`, `blobs`, `runs`, or `jwt_used`.

Production object storage also needs provider-side lifecycle on the
same prefix: `deploy/spaces/actions-lifecycle.json` expires
`actions/runs/` objects after 90 days and aborts stale multipart
uploads after 2 days. Apply it with
`deploy/cutover/apply-actions-lifecycle.sh`.

## Trigger pipeline (S41b)

Three layers between a triggering event and a queued `workflow_run`:

```
caller (push_process / pulls.Create / pr_jobs.PRSynchronize / dispatch HTTP)
    │
    └─► worker.Enqueue(KindWorkflowTrigger, JobPayload)
            │
            └─► trigger.Handler picks up:
                  Discover .shithub/workflows/*.yml at HEAD SHA
                  Parse each (skip + log on Error diagnostics)
                  Match each against trigger.Event
                  Enqueue each match
                        │
                        └─► trigger.Enqueue (one tx):
                              INSERT workflow_runs (ON CONFLICT DO NOTHING)
                              INSERT workflow_jobs per parsed job
                              INSERT workflow_steps per parsed step
                              (commit)
                              checks.Create per job (post-tx, idempotent
                                via ExternalID 'workflow_run:<id>:job:<key>')
```

### Idempotency on the triggering event

The robust pattern, not a UNIQUE on `(repo_id, head_sha)`. Each
caller constructs a stable `trigger_event_id` from its triggering
event's identity:

| Caller              | trigger_event_id format                          |
| ------------------- | ------------------------------------------------ |
| push_process        | `push:<push_event_id>`                           |
| pulls.Create        | `pr_opened:<pr_id>:<head_sha>`                   |
| pr_jobs.PRSynchronize | `pr_synchronize:<pr_id>:<head_sha>`            |
| dispatch HTTP       | `dispatch:<file>:<sha>:<8-byte-random-hex>`      |
| schedule sweep (S41b-2) | `schedule:<workflow_id>:<window_start_unix>` |

Migration 0051 adds `workflow_runs.trigger_event_id` (text NOT NULL
DEFAULT '') with a partial UNIQUE on
`(repo_id, workflow_file, trigger_event_id) WHERE trigger_event_id <> ''`.
The trigger handler does `INSERT … ON CONFLICT DO NOTHING` so:

- Worker retries (the same push_process replay) → no duplicate runs.
- Admin replays via `shithubd admin run-job workflow:trigger ...`
  → no duplicate runs.
- Re-runs explicitly construct a NEW
  trigger_event_id (`rerun:<original_run_id>:<request_uuid>`) and
  chain back via `parent_run_id`. History is preserved, no
  collision.

Each caller's collision-free namespace is short-lived and
human-debuggable: a Postgres operator can grep
`workflow_runs.trigger_event_id` to see exactly which triggering
event produced a given run.

### Filter evaluation

`trigger.Match(workflow, event)` is a pure function (no I/O, no DB).
For each event kind:

- **push**: branch vs tag classified from the ref; only the matching
  filter list applies (a `branches:` filter rejects tag pushes and
  vice versa). `paths:` (when set) requires at least one changed
  path to match. Empty filter = match-all.
- **pull_request**: `types:` defaults to
  `[opened, synchronize, reopened]` when omitted (GHA parity).
  `branches:` applies to the **base** ref. `paths:` as for push.
- **schedule**: requires the workflow to declare the cron expression
  that fired. The sweep is the source of truth for which cron
  fires; we just gate on declaration. Avoids interpreting cron
  semantics in two places.
- **workflow_dispatch**: matches whenever the workflow declares
  `on.workflow_dispatch`.

Glob semantics in `branches:`/`tags:`/`paths:`: minimatch subset
with `*` (single segment), `**` (any), `/**` end-anchor (optional
trailing path), `**/` start-anchor, and `!exclude` (last-match-wins,
exclusion-only list implies include-all).

### Collaborator gate

Per the S41b spec's "external-PR support is parked" decision: PR
triggers (both `opened` and `synchronize`) only fire when the PR's
author is the repo's owning user. Conservative — drops legitimate
non-owner collaborators in the org-repo case. Expanding the gate
requires plumbing `policy.Can` into the worker context, which we
defer to S41g where the lifecycle work touches that surface anyway.

### Operator surface

- `POST /{owner}/{repo}/actions/workflows/{file}/dispatches`
  Body: `{"ref": "...", "inputs": {"key": "value"}}` (both optional;
  ref defaults to the repo's default branch). Returns 204 No Content
  on success. Synchronous trigger.Enqueue (no discovery — file is
  named in the URL). Auth: requires repo write.
- `GET /{owner}/{repo}/actions.atom`
  Returns the last 50 workflow runs as an Atom feed. Auth and visibility
  match the Actions tab (`repo:read`). Entries link to
  `/{owner}/{repo}/actions/runs/{run_index}` and include the workflow
  name/path, event, branch, short SHA, status, and conclusion.

### Webhook events (S41h)

Actions emits webhook-facing domain events through `notif.EmitTx` on
state transitions:

- `workflow_run`, with `payload.action` set to `queued`, `running`, or
  `completed` (`completed` may carry `conclusion:"cancelled"`).
- `workflow_job`, with `payload.action` set to `queued`, `running`,
  `completed`, or `cancelled`.

Payloads are structural snapshots only. They include ids, run index,
workflow path/name, head SHA/ref, event kind, status, conclusion,
timestamps, job key/name/runner id, needs, timeout, and cancellation
state. They deliberately exclude `workflow_runs.event_payload`, env,
permissions, logs, runner JWTs, and secret values. This keeps the
webhook surface stable without turning arbitrary workflow input into
subscriber-facing data.

### What S41b deliberately doesn't do

- Run jobs. S41c adds runner claim/status APIs; S41d adds the actual
  `shithubd-runner` execution binary.
- Schedule sweep. Cron-driven triggers split into S41b-2 to keep
  this PR reviewable; the trigger pipeline accepts schedule events,
  but no caller produces them yet. S41b-2 adds the sweep + the
  `robfig/cron/v3` dep + `shithubd-cron.service` wiring.
- External-PR triggers. Conservative collaborator gate above.

## Secrets + variables settings surface (S41c)

S41c wires the previously schema-only `workflow_secrets` and
`actions_variables` tables into repo/org settings.

Repository routes are gated through
`policy.ActionRepoSettingsActions` (`repo:settings:actions`, admin
role minimum):

- `GET /{owner}/{repo}/settings/secrets/actions`
- `POST /{owner}/{repo}/settings/secrets/actions`
- `POST /{owner}/{repo}/settings/secrets/actions/{name}/delete`
- `GET /{owner}/{repo}/settings/variables/actions`
- `POST /{owner}/{repo}/settings/variables/actions`
- `POST /{owner}/{repo}/settings/variables/actions/{name}/delete`

Organization routes follow the existing org-settings prefix and are
owner-only:

- `GET /organizations/{org}/settings/secrets/actions`
- `POST /organizations/{org}/settings/secrets/actions`
- `POST /organizations/{org}/settings/secrets/actions/{name}/delete`
- `GET /organizations/{org}/settings/variables/actions`
- `POST /organizations/{org}/settings/variables/actions`
- `POST /organizations/{org}/settings/variables/actions/{name}/delete`

Secrets are sealed through `internal/auth/secretbox` using the
operator-managed `Auth.TOTPKeyB64` root key. Secret list pages render
names/metadata only; the plaintext value is accepted once on create or
rotation and never rendered back. Variables are non-secret plaintext
configuration, so settings pages render their values. Both stores use
the same name grammar as the database constraints:
`^[A-Za-z_][A-Za-z0-9_]*$`, 1-100 characters. Variables additionally
enforce the 4096-character value cap in Go before hitting the DB
constraint.

SP23 adds environment-scoped secrets to the same encrypted table. A
job receives those rows only when its parsed `environment` name matches
a configured `repo_environments` row for the repository. Resolution
order is org/user → repo → environment, so environment secrets shadow
broader scopes for deploy-only credentials. Pull request runs still get
no secrets from any scope.

## What S41a deliberately doesn't do

- No trigger pipeline. `domain_events` aren't matched against `on:`
  yet — that's S41b.
- No runner. S41c/S41d add runner claim APIs and the execution binary.
- No UI. The Actions tab still renders the placeholder — S41f.
- No secret encryption helpers wired to anything writable — S41c.
- No JWT issuance, no runner registration flow — S41c.
- No log streaming, no SSE — S41d/f.
- No execution sandbox, no scrubbing, no injection guards
  *enforced at the runner* — S41d/e (the parser-side taint contract
  is the foundation those depend on, not a substitute).

## Why these choices, in two paragraphs

The schema work is front-loaded so later sub-sprints don't ripple a
migration through every PR. `version` (optimistic locking) and
`run_index` (per-repo monotonic) are the two columns I'd flag to a
new maintainer immediately — both are nearly free to add up front
and painful to retrofit. The split between hot-path log chunks
(Postgres) and finalized blob (Spaces) is shaped after Forgejo's
log path; we pick the boring well-trodden answer over the clever
one because log throughput is the failure mode that bites first.

The taint contract is the security-load-bearing piece. Every later
sub-sprint trusts that the `Tainted` flag is set correctly here, in
the parser/evaluator, and never re-derived downstream. The narrow
allowlist of namespaces and functions exists exactly so a future PR
that adds, say, `fromJSON` has to do it knowingly — by widening the
allowlist in a visible diff, with a reviewer-required note, rather
than by accident. The `${{ github.* }}` alias is a pragmatic
concession to copy-paste users; the rebrand to `${{ shithub.* }}`
is the canonical form so future divergence isn't awkward.

## See also

- `internal/actions/workflow/parse.go` — the parser
- `internal/actions/expr/eval.go` — the evaluator
- `internal/migrationsfs/migrations/0042..0049_*.sql` — the schema
- `tests/fixtures/workflows/*.yml` — canonical input shapes
- `internal/actions/workflow/parse_test.go` — fixture-driven tests
- `internal/actions/expr/eval_test.go` — taint-contract tests
- `.refs/forgejo/services/actions/` — reference architecture
- Campaign plan in conversation memory (humble-cooking-bunny)
