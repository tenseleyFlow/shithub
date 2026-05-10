# Actions runner smoke runbook

This runbook drives one queued Actions job with curl. It is for S41c
operator validation before the real `shithubd-runner` binary lands.

Prereqs:

- Database migrations are current through `0053_runner_jwt_used.sql`.
- `SHITHUB_TOTP_KEY` or `auth.totp_key_b64` is set on the web process.
- Object storage is configured if testing artifact upload.
- A repo has a workflow under `.shithub/workflows/*.yml` with
  `runs-on: ubuntu-latest`, and a push/dispatch has enqueued a run.

Register a runner:

```sh
shithubd admin runner register \
  --name runner-1 \
  --labels self-hosted,linux,ubuntu-latest \
  --capacity 1
```

Save the printed token:

```sh
export RUNNER_TOKEN='<printed-token>'
export BASE='https://shithub.example'
```

Claim a job:

```sh
curl -fsS "$BASE/api/v1/runners/heartbeat" \
  -H "Authorization: Bearer $RUNNER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"labels":["self-hosted","linux","ubuntu-latest"],"capacity":1}' \
  | tee /tmp/shithub-claim.json
```

Extract the job token and id:

```sh
export JOB_ID="$(jq -r '.job.id' /tmp/shithub-claim.json)"
export JOB_TOKEN="$(jq -r '.token' /tmp/shithub-claim.json)"
```

Append a log chunk:

```sh
curl -fsS "$BASE/api/v1/jobs/$JOB_ID/logs" \
  -H "Authorization: Bearer $JOB_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"seq\":0,\"chunk\":\"$(printf 'hello from curl\n' | base64)\"}" \
  | tee /tmp/shithub-log.json

export JOB_TOKEN="$(jq -r '.next_token' /tmp/shithub-log.json)"
```

Complete the job:

```sh
curl -fsS "$BASE/api/v1/jobs/$JOB_ID/status" \
  -H "Authorization: Bearer $JOB_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"status":"completed","conclusion":"success"}'
```

Replay check: reusing the log token after the log call must fail with
401 because its `jti` is already present in `runner_jwt_used`.

```sh
curl -i "$BASE/api/v1/jobs/$JOB_ID/status" \
  -H "Authorization: Bearer $(jq -r '.next_token' /tmp/shithub-log.json)" \
  -H "Content-Type: application/json" \
  -d '{"status":"running"}'
```

Expected results:

- `workflow_jobs.status = completed` and conclusion `success`.
- The parent `workflow_runs` row rolls up to completed/success when all
  jobs are terminal.
- The PR Checks tab shows the matching check run as success.
- `/metrics` includes runner registration, heartbeat, and JWT counters.
