# Actions runner smoke runbook

This runbook validates the runner-facing Actions path. `shithubd-runner`
now claims jobs, performs scoped `actions/checkout@v4`, and executes
containerized `run:` steps through Docker or Podman. The curl flow below
remains useful for token/replay debugging.

For host provisioning and the systemd/Ansible path, see
[runner-deploy.md](./runner-deploy.md).

Prereqs:

- Database migrations are current through `0057_workflow_job_secret_masks.sql`.
- `SHITHUB_TOTP_KEY` or `auth.totp_key_b64` is set on the web process.
- Object storage is configured if testing artifact upload.
- Docker or Podman is installed on the runner host.
- A repo has a workflow under `.shithub/workflows/*.yml` with
  `runs-on: ubuntu-latest`, and a push/dispatch has enqueued a run.
  Checkout and `run:` steps are executable; artifact aliases remain
  reserved until artifact transfer lands.

`runs-on` is a runner-label selector, not a hard-coded image name.
A workflow that says `runs-on: ubuntu-latest` can be claimed by any
runner advertising the `ubuntu-latest` label. The container image is
selected by the runner host's `engine.default_image` setting; the
reproducible Nix-built image is the default, but operators can point it
at another OCI image when they need closer Ubuntu parity.

Register a runner:

```sh
shithubd admin runner register \
  --name runner-1 \
  --labels self-hosted,linux,ubuntu-latest,x64 \
  --capacity 1 \
  --output json
```

Save the returned token:

```sh
export RUNNER_TOKEN='<printed-token>'
export BASE='https://shithub.example'
```

Run the binary:

```sh
shithubd-runner run \
  --server-url "$BASE" \
  --token "$RUNNER_TOKEN" \
  --labels self-hosted,linux,ubuntu-latest,x64 \
  --workspace-root /var/lib/shithubd-runner/workspaces \
  --network shithub-actions \
  --dns-servers 172.30.0.1
```

## Pool Operations

List pool state:

```sh
shithubd admin runner list --output json
shithubd admin runner queue --output json
```

`list` includes labels, capacity, active job count, last heartbeat,
host name, runner version, drain state, and revoke state. `queue`
groups queued jobs by requested `runs-on` label so unsupported labels
are visible without querying Postgres.

Drain a runner before host maintenance:

```sh
shithubd admin runner drain --id 7 --reason 'kernel update'
```

The runner keeps heartbeating and can finish already claimed jobs, but
new heartbeat claims return 204 until it is undrained:

```sh
shithubd admin runner undrain --id 7
```

Rotate a registration token after a config-management change:

```sh
shithubd admin runner rotate-token --id 7 --expires-in 24h --output json
```

Update the runner host config with the printed token, restart
`shithubd-runner`, then verify heartbeats with `runner list`.

Mark stale runners offline:

```sh
shithubd admin runner cleanup-stale --older-than 2m
```

Hard-revoke a compromised runner:

```sh
shithubd admin runner revoke --id 7 --reason 'host compromise'
```

Revocation records `revoked_at`, marks the runner offline, revokes every
registration token for that runner, and causes existing job API JWTs from
that runner to fail. Use drain for routine maintenance; use revoke when
the token or host may be compromised.

Destroy/recreate with DigitalOcean:

```sh
doctl compute droplet list --tag-name shithub-actions-runner
doctl compute droplet delete <droplet-id>
```

After recreation, register a fresh runner token and let the provisioning
role write the new config. Do not reuse a token from a revoked runner.

Emergency controls:

- Stop all new claims: disable Actions at site level or drain every
  runner with `shithubd admin runner list --output json` followed by
  `shithubd admin runner drain --id <id>`.
- Pause one repo/org: set the repo/org Actions policy to disabled so the
  claim query leaves its queued jobs untouched.
- Cancel active work for a repo: use the repo Actions UI or job cancel
  API for each queued/running job.
- Revoke all runner tokens: iterate `shithubd admin runner revoke --id
  <id>` over every non-revoked runner.
- Fence the pool: block or destroy droplets tagged
  `shithub-actions-runner` in DigitalOcean, then rotate or revoke the
  affected runner tokens before allowing replacement hosts to connect.

Equivalent config file:

```toml
[server]
base_url = "https://shithub.example"

[runner]
token = "<printed-token>"
labels = ["self-hosted", "linux", "ubuntu-latest", "x64"]
capacity = 1
poll_interval = "5s"
workspace_root = "/var/lib/shithubd-runner/workspaces"
workspace_ttl = "24h"
network_allowlist = [
  "api.github.com",
  "auth.docker.io",
  "codeload.github.com",
  "github.com",
  "objects.githubusercontent.com",
  "production.cloudflare.docker.com",
  "registry-1.docker.io",
  "*.githubusercontent.com",
]

[engine]
kind = "docker"
default_image = "ghcr.io/shithub/runner-nix:1.0"
network = "shithub-actions"
memory = "2g"
cpus = "2"
seccomp_profile = "/etc/shithubd-runner/seccomp.json"
user = "65534:65534"
pids_limit = 512
dns_servers = ["172.30.0.1"]
```

The config path defaults to `/etc/shithubd-runner/config.toml`.
Environment variables use the `SHITHUB_RUNNER_` prefix, for example
`SHITHUB_RUNNER_TOKEN` or `SHITHUB_RUNNER_SERVER__BASE_URL`.
Use `--expires-in` only for tokens that your automation rotates before expiry;
the runner presents its registration token on every heartbeat.

The Ansible runner role creates the `shithub-actions` bridge, runs the
allowlist resolver at `172.30.0.1`, and installs firewall rules that
reject direct-IP egress from step containers. If you run the binary
without the role, provision equivalent network controls before pointing
workflows at the runner.

## Curl token smoke

Claim a job:

```sh
curl -fsS "$BASE/api/v1/runners/heartbeat" \
  -H "Authorization: Bearer $RUNNER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"labels":["self-hosted","linux","ubuntu-latest","x64"],"capacity":1,"host_name":"curl-smoke","version":"manual"}' \
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

Cancel smoke: on a separate queued or running job, a repo-write PAT can
request cancellation:

```sh
curl -fsS "$BASE/api/v1/jobs/$JOB_ID/cancel" \
  -H "Authorization: Bearer $PAT_WITH_REPO_WRITE" \
  -X POST
```

If the job is still queued, it becomes `cancelled` immediately. If it is
running, the next runner cancel check returns `{"cancelled":true}`, the
runner kills the active container, and the terminal job status becomes
`cancelled`.

Re-run smoke: after a completed or cancelled workflow run, a repo-write
PAT can enqueue a new run from the original workflow file at the
original commit:

```sh
curl -fsS "$BASE/api/v1/runs/$RUN_ID/rerun" \
  -H "Authorization: Bearer $PAT_WITH_REPO_WRITE" \
  -X POST
```

Expected response includes a new `run_id`, the new `run_index`, and
`parent_run_id` equal to the source run. Confirm the new row has the
same `head_sha` as the source run.

Timeout smoke: create a workflow with a short deadline and a long-running
step:

```yaml
jobs:
  timeout_probe:
    runs-on: ubuntu-latest
    timeout-minutes: 1
    steps:
      - run: sleep 600
```

Expected results:

- The runner kills the active container shortly after the one-minute
  deadline.
- The step row becomes `status=completed`,
  `conclusion=timed_out`.
- The job row becomes `status=completed`, `conclusion=timed_out`.
- `/metrics` increments `shithub_actions_step_timeouts_total`.

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
- `/metrics` includes runner registration, heartbeat, JWT, job
  cancellation, log-scrub, step-timeout, retention, queue-depth by label,
  claim-latency, runner-online, runner-stale, runner-draining, and
  runner-revocation metrics.

## Retention Sweep

The daily housekeeping timer enqueues `workflow:cleanup` at 03:30 UTC,
after the 03:17 backup window:

```sh
systemctl list-timers shithubd-cron.timer
journalctl -u shithubd-cron.service -n 100
```

Manual smoke:

```sh
shithubd admin run-job workflow:cleanup
```

Expected behavior:

- SQL log chunks older than 7 days for terminal steps are deleted.
- Expired artifact rows are deleted only after their `actions/runs/...`
  objects are deleted from object storage.
- Unpinned terminal workflow runs older than 365 days are pruned;
  pinned runs survive.
- Consumed runner JWT rows older than 30 days are pruned.
- `/metrics` exposes
  `shithub_actions_runs_pruned_total{kind="chunks|blobs|runs|jwt_used"}`.
