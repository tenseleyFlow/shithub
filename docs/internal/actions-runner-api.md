# Actions runner API

The runner-facing HTTP surface lives in
`internal/web/handlers/api/runners.go`. It is mounted under `/api/v1`
in the CSRF-exempt API group, but it does not use PAT auth. Runners
authenticate first with a long-lived registration token and then with
short-lived per-job JWTs.

## Auth model

Operators register a runner with:

```sh
shithubd admin runner register \
  --name runner-1 \
  --labels self-hosted,linux,ubuntu-latest,x64 \
  --capacity 1 \
  --output json
```

The command inserts `workflow_runners`, stores only a SHA-256 hash in
`runner_tokens`, and returns the raw 32-byte hex token once.
`--expires-in` is optional and should only be used when the deployment rotates
the runner token before it expires, because the runner uses that same token for
heartbeat authentication.

Operators can drain, undrain, rotate, and hard-revoke runners with
`shithubd admin runner drain`, `undrain`, `rotate-token`, and `revoke`.
Drained runners keep heartbeating and may finish already claimed jobs,
but heartbeat claims return 204 until the runner is undrained. Hard
revocation sets the runner offline, records `revoked_at`, revokes all
registration tokens, and makes job API JWTs minted for that runner
invalid. This is the token-compromise boundary: a host with an old config
file cannot claim new jobs or update already claimed jobs after
revocation lands in Postgres.

`POST /api/v1/runners/heartbeat` accepts:

```http
Authorization: Bearer <registration-token>
```

When a queued job matches the runner labels and capacity is available,
the response includes a job payload and a 15-minute job JWT. That JWT
has claims:

```json
{"sub":"runner:<id>","purpose":"api","job_id":1,"run_id":1,"repo_id":1,"exp":0,"jti":"..."}
```

The signing key is derived from `auth.totp_key_b64` with HKDF label
`actions-runner-jwt-v1`; the raw TOTP/secretbox key is not used
directly for JWT signing.

API-purpose job JWTs are single-use. Every job endpoint verifies the
signature and expiry, checks that the path job belongs to the claimed
runner/run, and then inserts `jti` into `runner_jwt_used`. A replay
returns 401. To support multi-step runner flows, successful in-flight job
endpoints return `next_token` and `next_token_expires_at`.

Consumed JWT rows are retained for 30 days after token expiry, then
pruned by the daily `workflow:cleanup` worker. This keeps the replay
gate audit trail available for recent jobs without letting the table
grow unbounded.

`shithubd-runner` consumes the same token chain: it claims with the
registration token, marks the job `running` with the first API-purpose job
JWT, then uses each returned `next_token` serially for log chunks,
step-status updates, cancel checks, artifact upload requests, and finally
the terminal job-status update. Reusing any consumed API-purpose job JWT
is a replay and must fail with 401.

The heartbeat claim also returns `job.checkout_url` and
`job.checkout_token` for `actions/checkout@v4`. The checkout token is a
separate JWT with `purpose:"checkout"` and the same runner/job/run/repo
scope. It is intentionally reusable while the job is `running`, because
Git smart HTTP performs multiple Basic-authenticated requests during one
checkout. The git HTTP handler accepts it only for `git-upload-pack`, only
for the claimed repository, and only while the database still shows that
the claimed runner is running the job. It is never accepted for pushes or
runner API endpoints. The runner first fetches the exact workflow
`head_sha`; if the server rejects that object as unadvertised, the runner
falls back to fetching the safe `refs/heads/*` or `refs/tags/*` `head_ref`
without a shallow depth and still verifies that `git rev-parse HEAD` equals
the expected `head_sha` before running any later step.

## Endpoints

`POST /api/v1/runners/heartbeat`

Request body:

```json
{
  "labels": ["self-hosted", "linux", "ubuntu-latest", "x64"],
  "capacity": 1,
  "host_name": "runner-host-1",
  "version": "v0.1.0",
  "active_job_ids": []
}
```

Returns 204 when no matching job is claimable. Returns 200 with
`token`, `expires_at`, and `job` when a job is claimed. Capacity is
enforced server-side by counting current `workflow_jobs.status =
'running'` rows for the runner while holding a row lock on the runner.
Claiming also enforces the effective Actions policy for the repository:
disabled repos, approval-pending runs, per-repo concurrent job caps, and
per-owner/org concurrent job caps are not dispatchable. Approval simply
sets `workflow_runs.approved_by_user_id`; the next heartbeat can claim the
same queued jobs, so no duplicate run is created.
`host_name` and `version` are optional runner metadata. The server stores
trimmed values up to 255 bytes for pool diagnostics and preserves the
previous values when old runners omit them.
`active_job_ids` is the runner's authoritative local active-job set. An
empty array means the runner is idle. When the field is present, the
server first cancels any `running` jobs assigned to that runner whose IDs
are missing from the set, rolls up their workflow runs, and syncs their
check runs. This recovers capacity after a runner restart, lost job
token, or local process failure leaves the database with a stale running
row. Omitted or `null` `active_job_ids` is treated as an old runner
heartbeat and does not reconcile jobs.
The job payload includes `checkout_url`, `checkout_token`, resolved
`secrets`, and `mask_values`; repo secrets shadow org secrets with the
same name. The server also stores an encrypted claim-time copy of the mask
values on `workflow_job_secret_masks` so later log uploads are scrubbed
against the secrets that were actually handed to the runner, even if an
operator rotates or deletes a secret mid-job.

Pull request runs receive no org or repo secrets in v1, even after a
maintainer approves dispatch. This is intentionally stricter than the
approval gate until environments/protected deployment secrets exist.

`POST /api/v1/jobs/{id}/logs`

Auth: job JWT. Body:

```json
{"seq":0,"chunk":"aGVsbG8K","step_id":123}
```

`step_id` is optional for the S41c curl smoke path; when omitted the
first step in the job receives the chunk. Chunks are base64-decoded,
capped at 512 KiB raw, and appended to `workflow_step_log_chunks`.
Duplicate `(step_id, seq)` inserts are accepted as idempotent retries.
Before append, the API re-scrubs exact secret values from the job's
claim-time mask snapshot. It also reprocesses any possible secret prefix
carried at the end of the prior chunk, so a runner cannot leak a secret
by splitting it across two log calls.

After the server-side scrub, complete workflow-command lines of the form
`::notice ...::message`, `::warning ...::message`, and
`::error ...::message` are parsed into `workflow_annotations`. Annotation
title, message, and path fields are capped, UTF-8 repaired, stripped of
terminal/control sequences, and redacted against the same mask snapshot before
storage. A trailing non-newline-terminated line is ignored until a later chunk
completes it so partial secret fragments are not persisted as annotations.

`POST /api/v1/jobs/{id}/steps/{step_id}/status`

Auth: job JWT. Body:

```json
{"status":"completed","conclusion":"success"}
```

Valid transitions are `queued|running -> running|completed|cancelled|skipped`
with idempotent repeats of the target terminal state. Completed and
skipped steps require a valid check conclusion; cancelled defaults to
`cancelled` when omitted. The endpoint always returns a `next_token`
because a completed step is not the end of the job.

When object storage is configured, terminal step updates enqueue
`workflow:finalize_step`. The worker concatenates
`workflow_step_log_chunks` in sequence order, uploads the log to
`actions/runs/<run_id>/jobs/<job_id>/steps/<step_id>.log`, stores that
key and byte count on `workflow_steps`, then deletes the SQL chunks.

The repository Actions UI reads logs from the same two-stage storage
model. While chunks remain in SQL, a step log page concatenates them in
sequence order and renders a static snapshot. After finalization, the
page reads `workflow_steps.log_object_key` from object storage and
renders the same capped snapshot from object storage. Downloads stream through
shithub with `Content-Disposition: attachment` for both SQL-backed and
object-backed logs; browser pages do not expose raw object-storage URLs. Live
tailing uses the S41f SSE route while chunks remain hot.

Completed/non-streaming step-log pages pass the capped snapshot through
`internal/actions/logview` for display only. The parser recognizes
`::group::<title>` and `::endgroup::` markers as collapsible sections and emits
deterministic `L<N>` anchors for visible lines. Titles and lines remain text and
are escaped by `html/template`; raw downloads and live SSE chunks are not
rewritten by this view parser.

`POST /api/v1/jobs/{id}/status`

Auth: job JWT. Body:

```json
{"status":"completed","conclusion":"success"}
```

Valid transitions are `queued|running -> running|completed|cancelled`.
Completed jobs require a valid check conclusion. The handler updates
`workflow_jobs`, rolls up `workflow_runs`, and best-effort updates the
matching `check_runs` row created by the trigger pipeline.

`timeout-minutes` is enforced by `shithubd-runner` as a whole-job
deadline. When it expires, the runner kills the active container,
reports the current step as `completed/timed_out`, and reports the job
as `completed/timed_out`. The server treats that conclusion as terminal
failure for the workflow run rollup.

When a runner reports `status:"cancelled"`, any still-open steps in the
job are marked cancelled too. This keeps a killed job from leaving queued
step rows that the UI would otherwise treat as live.

Runner execution supports host-side `actions/checkout@v4`, first-party
`actions/setup-python@v5`, and containerized `run:` steps with per-step log
streaming and server-side log finalization. Setup-python is resolved locally
from the runner image/toolcache and mutates only the in-memory job environment
for later steps. Artifact upload/download aliases remain reserved until the
artifact transfer path lands.

`POST /api/v1/jobs/{id}/artifacts/upload`

Auth: job JWT. Body:

```json
{"name":"test-results.tgz","size_bytes":12345}
```

Creates a `workflow_artifacts` row and returns a pre-signed S3 PUT URL.
The object key is `actions/runs/<run_id>/artifacts/<name>`.

`POST /api/v1/jobs/{id}/cancel`

Auth: PAT with `repo:write`, and the actor must have write permission on
the repository that owns the job's workflow run. Browser UI forms use
CSRF-protected repo routes that call the same lifecycle orchestrator.

Queued jobs are made terminal immediately:

- `workflow_jobs.status = cancelled`
- `workflow_jobs.conclusion = cancelled`
- `workflow_jobs.cancel_requested = true`
- open steps for that job are marked cancelled

Running jobs keep `status = running` and get
`cancel_requested = true`. The runner sees this through
`cancel-check`, kills the active container, then reports terminal
`cancelled`.

`POST /api/v1/runs/{id}/rerun`

Auth: PAT with `repo:write`, and the actor must have write permission on
the repository that owns the workflow run. Browser UI forms use
CSRF-protected repo routes for the same operation.

Only terminal workflow runs are rerunnable. A re-run reads the original
workflow file from the source run's `head_sha`, not from the current
branch tip, then enqueues a new `workflow_runs` row with:

- the same `repo_id`, `workflow_file`, `head_sha`, `head_ref`, event,
  and event payload
- `actor_user_id` set to the user requesting the re-run
- `parent_run_id` set to the source run
- a fresh `trigger_event_id` in the `rerun:<source_run_id>:<random>`
  namespace

`POST /api/v1/jobs/{id}/cancel-check`

Auth: job JWT. Returns:

```json
{"cancelled":false,"next_token":"..."}
```

The boolean mirrors `workflow_jobs.cancel_requested`. `shithubd-runner`
polls this endpoint during job execution, serializing it through the
same single-use JWT chain as logs and status updates. On `cancelled:
true`, the Docker engine runs `docker kill <active-container>` and the
runner posts terminal job status `cancelled`.

## Metrics

- `shithub_actions_runner_registrations_total`
- `shithub_actions_runner_heartbeats_total{result="claimed|no_job"}`
- `shithub_actions_runner_jwt_total{result="issued|rejected|replay"}`
- `shithub_actions_queue_depth{resource="runs|jobs"}`
- `shithub_actions_active{resource="runs|jobs"}`
- `shithub_actions_runner_heartbeat_age_seconds{runner,status}`
- `shithub_actions_runner_capacity{runner,status}`
- `shithub_actions_runs_completed_total{event,conclusion}`
- `shithub_actions_run_duration_seconds{event,conclusion}`
- `shithub_actions_steps_completed_total{step_type,conclusion}`
- `shithub_actions_jobs_cancelled_total{reason="user|concurrency|timeout|runner_lost"}`
- `shithub_actions_concurrency_queued_total`
- `shithub_actions_log_scrub_replacements_total{location="server"}`
- `shithub_actions_log_chunks_total{location="server"}`
- `shithub_actions_log_chunk_bytes_total{location="server"}`
- `shithub_actions_runs_pruned_total{kind="chunks|blobs|runs|jwt_used"}`
- `shithub_actions_step_timeouts_total`
- `shithub_actions_storage_objects{kind="artifacts|step_logs|hot_log_chunks"}`
- `shithub_actions_storage_bytes{kind="artifacts|step_logs|hot_log_chunks"}`

The gauge observer refreshes queue, runner and object-count gauges every
15 s. `shithub_actions_storage_bytes{kind="hot_log_chunks"}` is refreshed
every 5 min instead: summing `octet_length(chunk)` detoasts every row of
`workflow_step_log_chunks`, so it is scraped on a slower cadence and can
lag the other gauges by up to five minutes.
