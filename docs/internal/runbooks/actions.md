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
containerized run: steps -> log chunks -> step/job status -> run rollup
```

The v1 executor supports containerized `run:` steps. The parser reserves
`actions/checkout@v4`, `shithub/upload-artifact@v1`, and
`shithub/download-artifact@v1`, but the Docker runner rejects `uses:` steps
until checkout metadata and artifact transfer are wired end to end. Do not use
`actions/checkout@v4` in production smoke workflows yet.

## First smoke

1. Confirm migrations are applied and the web process can enqueue workers.
2. Register one runner with a label that matches the workflow:

```sh
shithubd admin runner register \
  --name smoke-runner-1 \
  --labels self-hosted,linux,ubuntu-latest \
  --capacity 1
```

3. Start `shithubd-runner` with the printed token. For production hosts, use
   the Ansible/systemd path in [runner-deploy.md](./runner-deploy.md).
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

Important metrics:

- `shithub_actions_runner_heartbeats_total{result="claimed|no_job"}`
- `shithub_actions_runner_jwt_total{result="issued|rejected|replay"}`
- `shithub_actions_jobs_cancelled_total{reason="user|concurrency|timeout"}`
- `shithub_actions_log_scrub_replacements_total{location="server"}`
- `shithub_actions_step_timeouts_total`

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
  and verify the trigger event matches `on:`.
- **Run stays queued:** confirm a runner is registered with matching labels and
  capacity, then inspect runner journal output and heartbeat metrics.
- **Step logs buffer:** verify the Caddy route above and confirm the SSE route
  is still mounted outside compression and short timeouts.
- **`uses:` step fails:** expected for now. Replace with a `run:` step until
  checkout/artifact support lands.
- **Secrets appear masked inconsistently:** check
  `shithub_actions_log_scrub_replacements_total{location="server"}` and confirm
  the job was claimed after the secret was created or rotated. Mask snapshots
  are captured at claim time.
