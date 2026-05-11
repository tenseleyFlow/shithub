# Actions runner API

The runner-facing HTTP surface lives in
`internal/web/handlers/api/runners.go`. It is mounted under `/api/v1`
in the CSRF-exempt API group, but it does not use PAT auth. Runners
authenticate first with a long-lived registration token and then with
short-lived per-job JWTs.

## Auth model

Operators register a runner with:

```sh
shithubd admin runner register --name runner-1 --labels self-hosted,linux,ubuntu-latest
```

The command inserts `workflow_runners`, stores only a SHA-256 hash in
`runner_tokens`, and prints the 32-byte hex token once.

`POST /api/v1/runners/heartbeat` accepts:

```http
Authorization: Bearer <registration-token>
```

When a queued job matches the runner labels and capacity is available,
the response includes a job payload and a 15-minute job JWT. That JWT
has claims:

```json
{"sub":"runner:<id>","job_id":1,"run_id":1,"repo_id":1,"exp":0,"jti":"..."}
```

The signing key is derived from `auth.totp_key_b64` with HKDF label
`actions-runner-jwt-v1`; the raw TOTP/secretbox key is not used
directly for JWT signing.

Job JWTs are single-use. Every job endpoint verifies the signature and
expiry, checks that the path job belongs to the claimed runner/run, and
then inserts `jti` into `runner_jwt_used`. A replay returns 401. To
support multi-step runner flows, successful in-flight job endpoints
return `next_token` and `next_token_expires_at`.

`shithubd-runner` consumes the same token chain: it claims with the
registration token, marks the job `running` with the first job JWT, then
uses each returned `next_token` serially for log chunks, step-status
updates, cancel checks, artifact upload requests, and finally the
terminal job-status update. Reusing any consumed job JWT is a replay and
must fail with 401.

## Endpoints

`POST /api/v1/runners/heartbeat`

Request body:

```json
{"labels":["ubuntu-latest","linux"],"capacity":1}
```

Returns 204 when no matching job is claimable. Returns 200 with
`token`, `expires_at`, and `job` when a job is claimed. Capacity is
enforced server-side by counting current `workflow_jobs.status =
'running'` rows for the runner while holding a row lock on the runner.
The job payload includes resolved `secrets` and `mask_values`; repo
secrets shadow org secrets with the same name. The server also stores
an encrypted claim-time copy of the mask values on
`workflow_job_secret_masks` so later log uploads are scrubbed against
the secrets that were actually handed to the runner, even if an
operator rotates or deletes a secret mid-job.

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
offers a short-lived signed download URL. Live tailing is intentionally
separate and lands in the S41f SSE slice.

`POST /api/v1/jobs/{id}/status`

Auth: job JWT. Body:

```json
{"status":"completed","conclusion":"success"}
```

Valid transitions are `queued|running -> running|completed|cancelled`.
Completed jobs require a valid check conclusion. The handler updates
`workflow_jobs`, rolls up `workflow_runs`, and best-effort updates the
matching `check_runs` row created by the trigger pipeline.

S41d PR2 runner execution supports containerized `run:` steps with
per-step log streaming and server-side log finalization. `uses:` aliases
such as `actions/checkout@v4` and artifact upload/download remain
reserved for the later S41d slices that add checkout metadata and
artifact transfer.

`POST /api/v1/jobs/{id}/artifacts/upload`

Auth: job JWT. Body:

```json
{"name":"test-results.tgz","size_bytes":12345}
```

Creates a `workflow_artifacts` row and returns a pre-signed S3 PUT URL.
The object key is `actions/runs/<run_id>/artifacts/<name>`.

`POST /api/v1/jobs/{id}/cancel-check`

Auth: job JWT. Returns:

```json
{"cancelled":false,"next_token":"..."}
```

The boolean mirrors `workflow_jobs.cancel_requested`; the actual cancel
request UI lands later in S41g.

## Metrics

- `shithub_actions_runner_registrations_total`
- `shithub_actions_runner_heartbeats_total{result="claimed|no_job"}`
- `shithub_actions_runner_jwt_total{result="issued|rejected|replay"}`
- `shithub_actions_log_scrub_replacements_total{location="server"}`
