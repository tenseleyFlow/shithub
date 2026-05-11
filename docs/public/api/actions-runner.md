# Actions runner API

The Actions runner API is mounted under `/api/v1`, but it is not a
personal-access-token surface. Operators register runners with
`shithubd admin runner register`; runner processes authenticate
heartbeats with that registration token and authenticate job updates
with short-lived, single-use job JWTs returned by the heartbeat claim.

These endpoints are intended for `shithubd-runner`, not browser clients
or third-party integrations.

## Runner heartbeat

```
POST /api/v1/runners/heartbeat
```

Claims one queued workflow job for a registered runner when labels and
capacity match. Returns `204 No Content` when no job is available, or
`200 OK` with a job payload and job JWT.

## Job log chunks

```
POST /api/v1/jobs/{id}/logs
```

Appends one base64-encoded log chunk to a workflow step. The runner uses
the `next_token` returned by each successful call for the next job API
request.

## Step status

```
POST /api/v1/jobs/{id}/steps/{step_id}/status
```

Marks a workflow step running, completed, cancelled, or skipped. Terminal
step updates enqueue server-side log finalization when object storage is
configured.

## Job status

```
POST /api/v1/jobs/{id}/status
```

Marks the claimed job running or terminal, rolls up the workflow run, and
updates the matching check run.

## Artifact upload

```
POST /api/v1/jobs/{id}/artifacts/upload
```

Creates a workflow artifact row and returns a short-lived signed PUT URL
for object storage.

## Cancel check

```
POST /api/v1/jobs/{id}/cancel-check
```

Returns whether the server has requested cancellation for the claimed
job.
