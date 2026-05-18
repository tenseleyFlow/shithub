# Actions runbook

This is the operator runbook for shithub Actions. Host provisioning lives in
[runner-deploy.md](./runner-deploy.md), and runner protocol details live in
[actions-runner-api.md](../actions-runner-api.md).

## Shape

```text
git push / workflow_dispatch / schedule / pull_request
        |
        v
workflow:trigger worker job
        |
        v
workflow_runs + workflow_jobs + workflow_steps + check_runs
        |
        v
registered runner heartbeat claims a matching queued job
        |
        v
actions/checkout@v4 -> containerized run: steps
        |
        v
log chunks -> step/job status -> run rollup
```

The v1 executor supports host-side `actions/checkout@v4` plus containerized
`run:` steps. The checkout token is short-lived, repository-scoped, tied to a
running job, and accepted only for read-only smart-HTTP fetches.
`shithub/upload-artifact@v1` and `shithub/download-artifact@v1` are still
reserved aliases and fail until artifact transfer is wired end to end.

## First smoke

1. Confirm migrations are applied and the web process can enqueue workers.
2. Register one runner with a label that matches the workflow:

```sh
shithubd admin runner register \
  --name smoke-runner-1 \
  --labels self-hosted,linux,ubuntu-latest,x64 \
  --capacity 1 \
  --output json
```

3. Start `shithubd-runner` with the returned token. For production hosts, use
   one token per host and store it in ansible-vault, host vars, or the deployment
   secret store. The role writes `/etc/shithubd-runner/config.toml` with
   restrictive permissions. Use `--expires-in` only when the automation rotates
   the runner token before that deadline; the current runner uses the
   registration token for every heartbeat.
4. Push a `run:`-only workflow:

```yaml
name: smoke
on: [push, workflow_dispatch]
jobs:
  hello:
    runs-on: ubuntu-latest
    env:
      RUN_ID: ${{ shithub.run_id }}
    steps:
      - run: echo "hello from shithub actions"
      - run: test -n "$RUN_ID"
```

5. Expected result:

- `workflow:trigger` enqueues a workflow run.
- A runner heartbeat claims the queued job within one idle poll interval.
- The Actions run page streams step logs while the job is running.
- The matching check run completes with `success`.
- `/{owner}/{repo}/actions.atom` includes the completed run.

Repeat with `exit 1`; the check should complete with `failure`.

## Checkout smoke

After the run-only smoke passes, verify repository checkout with:

```yaml
name: checkout smoke
on: [push, workflow_dispatch]
jobs:
  checkout:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: test -f README.md
      - run: test "$(git rev-parse HEAD)" = "${{ shithub.sha }}"
```

Expected result:

- The claim response contains `checkout_url` and `checkout_token`.
- Git smart HTTP sees the checkout token as Basic auth and permits
  `git-upload-pack` for the claimed repo.
- `git-receive-pack` rejects the same credential with 403.
- The job workspace contains the exact `shithub.sha` commit before the first
  `run:` step starts.

## Live log tail

Step log pages open an SSE stream at:

```text
/{owner}/{repo}/actions/runs/{run}/jobs/{job}/steps/{step}/log/stream
```

The stream sends `event: chunk` records with the chunk sequence as the SSE
`id`. Browsers reconnect with `Last-Event-ID`; the handler also accepts
`?after=<seq>` for the first connection from a rendered log page. A terminal
step sends `event: done` and closes the stream.

In `shithubd`, this route is mounted outside the normal app compression and
30-second timeout middleware. If a future route move puts live logs back under
either middleware, EventSource clients will churn and logs can buffer despite
the Caddy flush setting.

Log chunks are never sent through Postgres `NOTIFY`. Runner log writes append
to `workflow_step_log_chunks`, then `NOTIFY step_log_<step_id>` with only the
sequence number. Step completion notifies `done`.

## Rate limit

Live tails use `internal/ratelimit` scope `actions:logtail` with five
concurrent streams per viewer. Authenticated viewers key by user id; anonymous
public-repo viewers key by client IP. The limiter uses a short lease TTL so a
dropped connection cannot hold a slot permanently.

## Caddy

The production Caddy template has a dedicated Actions log-stream route with:

```caddy
flush_interval -1
```

The same route is excluded from gzip compression. If logs arrive only after
several kilobytes accumulate, verify the deployed `/etc/caddy/Caddyfile`
contains that route and reload Caddy:

```sh
sudo caddy reload --config /etc/caddy/Caddyfile
```

## Runner health

On the runner host:

```sh
systemctl status shithubd-runner
journalctl -u shithubd-runner -n 100 --no-pager
```

On the app host, inspect runner registration and heartbeat state:

```sh
shithubd admin actions runner list
```

Inspect queued jobs by requested `runs-on` label:

```sh
shithubd admin runner queue
shithubd admin runner queue --output json
```

Important metrics:

- `shithub_actions_queue_depth{resource="runs|jobs"}`
- `shithub_actions_active{resource="runs|jobs"}`
- `shithub_actions_runner_heartbeat_age_seconds{runner,status}`
- `shithub_actions_runner_capacity{runner,status}`
- `shithub_actions_runner_heartbeats_total{result="claimed|no_job"}`
- `shithub_actions_runner_jwt_total{result="issued|rejected|replay"}`
- `shithub_actions_runs_completed_total{event,conclusion}`
- `shithub_actions_run_duration_seconds{event,conclusion}`
- `shithub_actions_steps_completed_total{step_type,conclusion}`
- `shithub_actions_jobs_cancelled_total{reason="user|concurrency|timeout"}`
- `shithub_actions_log_scrub_replacements_total{location="server"}`
- `shithub_actions_log_chunks_total{location="server"}`
- `shithub_actions_log_chunk_bytes_total{location="server"}`
- `shithub_actions_storage_objects{kind="artifacts|step_logs|hot_log_chunks"}`
- `shithub_actions_storage_bytes{kind="artifacts|step_logs|hot_log_chunks"}`
- `shithub_actions_step_timeouts_total`

The committed dashboard JSON lives at:

```text
deploy/monitoring/grafana/dashboards/actions.json
```

## Load harness

`bench/k6/actions-load.js` exercises the runner HTTP API under concurrent job
claims. It does not create workflow runs itself; seed the queue first with
pushes or workflow dispatches that produce jobs matching the runner labels.

Required environment:

```sh
SHITHUB_BASE_URL=https://shithub.example.test \
SHITHUB_RUNNER_TOKENS=token-a,token-b,token-c \
k6 run bench/k6/actions-load.js
```

Useful knobs:

- `SHITHUB_ACTIONS_VUS=50` controls concurrent virtual users.
- `SHITHUB_ACTIONS_DURATION=10m` controls the steady-state window.
- `SHITHUB_RUNNER_LABELS=self-hosted,linux,ubuntu-latest,x64` sets heartbeat
  labels.
- `SHITHUB_RUNNER_CAPACITY=17` keeps three runners near the 50-concurrent
  target.
- `SHITHUB_ACTIONS_LOG_BYTES=4096` controls emitted log chunk size.

Healthy run expectations:

- queued jobs drain without unbounded `shithub_actions_queue_depth` growth;
- runner heartbeats keep advancing and no runner deadlocks;
- log append p99 stays below five seconds;
- retention metrics catch up after the retention sweep.

## Emergency cancel

Start with a dry run:

```sh
shithubd admin actions cancel-all --dry-run --limit 100
```

Scope to one repository when possible:

```sh
shithubd admin actions cancel-all --dry-run --repo-id 42
```

Then confirm:

```sh
shithubd admin actions cancel-all --confirm --repo-id 42
```

Queued jobs are marked cancelled immediately. Running jobs receive
`cancel_requested=true`; the runner sees that through `/cancel-check`, kills the
active container, and reports terminal `cancelled`.

## Common failures

- **Run never appears:** confirm the workflow file is under
  `.shithub/workflows/`, parse it with `shithubd admin actions parse <file>`,
  and verify the trigger event matches `on:`. If the owning organization is
  over its monthly Actions minutes quota, the trigger should now appear as a
  completed `action_required` run/check instead of disappearing.
- **Run stays queued:** open the run page to see the requested runner labels,
  then run `shithubd admin runner queue` and confirm a live runner is registered
  with matching labels and capacity. Unsupported hosted labels such as
  `windows-latest` and `macos-latest` intentionally remain queued until an
  operator registers matching runners.
- **Actions usage looks too high:** compare raw job wall-clock runtime with
  timeout-capped runtime. Billing and usage metrics cap completed/cancelled job
  runtime at `workflow_jobs.timeout_minutes`; a larger raw
  `completed_at - started_at` gap usually means a stale running row was
  cancelled later than the container actually stopped.
- **Step logs buffer:** verify the Caddy route above and confirm the SSE route
  is still mounted outside compression and short timeouts.
- **`actions/checkout@v4` fails:** confirm the job is still running, the repo
  URL in the runner claim points at this shithub instance, and the runner host
  can reach smart HTTP. The checkout token is not valid after the job leaves
  `running`. If logs mention an unadvertised object, confirm the runner binary
  includes the exact-SHA fallback: it should retry by fetching the safe
  `head_ref` and then verify the expected SHA before continuing.
- **Artifact `uses:` step fails:** expected for now. Replace with a `run:`
  step until artifact support lands.
- **Secrets appear masked inconsistently:** check
  `shithub_actions_log_scrub_replacements_total{location="server"}` and confirm
  the job was claimed after the secret was created or rotated. Mask snapshots
  are captured at claim time.
