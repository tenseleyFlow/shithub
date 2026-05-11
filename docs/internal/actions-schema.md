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

Actions migrations currently span 0042–0051 and 0053. Migration 0052 belongs to
the repo source-remotes feature and was already deployed before the runner JWT
replay table landed.

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
  expression. S41g's slot manager keys off this column.
- **`workflow_runs.parent_run_id`** — for re-runs. The new run
  references the original; the UI shows a "re-ran from #N" link.
- **`workflow_jobs.runner_id`** — FK added in 0046 (after the
  runners table exists). Nullable until claimed.
- **`workflow_steps`** has a CHECK constraint enforcing
  `(run_command IS NOT NULL) <> (uses_alias IS NOT NULL)` — exactly
  one of `run:` or `uses:`. The `uses_alias` column is further
  CHECK-constrained to the three magic aliases we accept in v1.
- **`workflow_secrets`** owns its value as `bytea` ChaCha20Poly1305-
  sealed via `internal/auth/secretbox`. Key derivation uses
  `cfg.Auth.TOTPKeyB64` (already an operator-managed root) +
  `(owner, kind, name)` salt so re-keying is per-row.
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

Exactly three aliases, no exceptions:

| Alias                            | What it does                              |
| -------------------------------- | ----------------------------------------- |
| `actions/checkout@v4`            | Clones the repo into the workspace        |
| `shithub/upload-artifact@v1`     | Uploads files to `workflow_artifacts`     |
| `shithub/download-artifact@v1`   | Pulls artifacts back in a downstream job  |

Any other `uses:` value (community actions, Docker images, composite
actions) is an Error-severity diagnostic. The marketplace problem is
explicitly out of scope for v1; revisit only if a real demand exists
and we have an answer for supply-chain trust.

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
| `secrets.X`      | workflow_secrets  | no (operator-controlled)    |
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
- Reading any other namespace → `Tainted: false`, except `env.X`
  preserves the taint of the resolved env value. This closes the
  escape where an event-derived value is first assigned to env and
  then interpolated through `${{ env.X }}`.
- Binary op (`==`, `!=`, `&&`, `||`) → tainted if either operand is.
- Unary op (`!`) → tainted iff its operand is.
- Function call (`contains`, `startsWith`, `endsWith`) → tainted
  if any argument is.

The runner consumes `Tainted` and refuses to interpolate tainted
values into shell strings. Instead, tainted values are bound to
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
the shell source string. Backticks, `$()`, `;`, `&&` — none of those
work as command-injection vectors when the value reaches the shell as
environment data instead of syntax.

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
payload and mask set, then applies the same exact-value scrub again in
the runner API before persisting chunks. The server path also carries a
possible secret-prefix tail from the prior persisted chunk, so a runner
that bypasses client-side scrubbing cannot leak a secret by splitting
it across adjacent log POSTs.

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
- S41g: `shithubd admin actions cancel <run-id>` flips
  `cancel_requested`.

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
- Re-runs (the future "Re-run" button) explicitly construct a NEW
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

### What S41b deliberately doesn't do

- Run jobs. S41c adds runner claim/status APIs; S41d adds the actual
  `shithubd-runner` execution binary.
- Schedule sweep. Cron-driven triggers split into S41b-2 to keep
  this PR reviewable; the trigger pipeline accepts schedule events,
  but no caller produces them yet. S41b-2 adds the sweep + the
  `robfig/cron/v3` dep + `shithubd-cron.service` wiring.
- External-PR triggers. Conservative collaborator gate above.
- `workflow_run` webhook events. S41h adds the webhook event family
  + atom feed.

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
