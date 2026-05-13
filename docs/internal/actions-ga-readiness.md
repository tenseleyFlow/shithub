# Actions GA readiness and dogfood decision

This is the S41h-5 pre-GA packet for shithub Actions. It records what is ready,
what remains intentionally deferred, and why shithub's full project CI is not
yet moved from GitHub Actions to shithub Actions.

For shared-pool public runner rollout, see
[`actions-public-runners.md`](./actions-public-runners.md). S41h covers whether
shithub can dogfood its own CI; S41j covers whether normal repositories can use
the production runner pool safely.

## Current decision

Do not move `.github/workflows/ci.yml` to `.shithub/workflows/ci.yml` in S41h.

The v1 runner is useful and dogfoodable, but it is not a drop-in GitHub Actions
runner. The current project CI uses:

- `actions/setup-go@v5`
- `golangci/golangci-lint-action@v8`
- GitHub-hosted runner caches and tool installation semantics

shithub Actions v1 intentionally accepts only:

- `actions/checkout@v4`
- `shithub/upload-artifact@v1`
- `shithub/download-artifact@v1`
- ordinary `run:` steps

The committed dogfood path is therefore
`.shithub/workflows/checkout-canary.yml`. That canary proves the self-hosted
trigger pipeline, runner claim, scoped checkout credential, containerized
`run:` execution, log path, check-run sync, and Actions UI without pretending
marketplace-action parity exists.

## Promotion criteria for full CI

Move the project's full CI to shithub Actions only after all criteria below are
true:

1. The default runner image or a first-party setup step provides Go, git, bash,
   `golangci-lint`, and any build tools required by `make ci`.
2. The workflow can be expressed with `actions/checkout@v4` plus `run:` steps,
   or shithub grows first-party equivalents for the missing setup/cache steps.
3. Required status checks on protected branches point at shithub check runs and
   preserve the same merge gate strength as the current GitHub-hosted CI.
4. Production deploy remains gated separately from untrusted pull-request code.
   Deploy secrets must not be available to fork/PR workflows.
5. The checkout canary and a production-like `make ci` workflow both pass on a
   trusted runner host.
6. The Actions load harness completes without queue starvation, runner
   deadlock, or log p99 above five seconds.
7. The pre-GA audit below has no open Critical or High findings.

## Evidence matrix

| Area | Evidence | Notes |
|---|---|---|
| Workflow parser and dialect | `internal/actions/workflow`, `docs/internal/actions-schema.md`, `docs/public/user/actions.md` | Unknown YAML keys and unsupported `uses:` aliases are rejected at parse time. |
| Trigger idempotency | `internal/actions/trigger`, `workflow_runs.trigger_event_id` | Retries and admin replays do not duplicate runs for the same triggering event. |
| Secrets and variables | `internal/actions/secrets`, `internal/actions/variables`, `workflow_job_secret_masks` | Secrets are AEAD-encrypted at rest; claim-time mask snapshots preserve log masking after rotation. |
| Runner JWT replay gate | `internal/auth/runnerjwt`, `runner_jwt_used`, `internal/web/handlers/api/runners.go` | Job API JWTs are short-lived and single-use. Checkout JWTs are separate and scoped to read-only git fetch. |
| Checkout credential scope | `internal/web/handlers/githttp` tests | Checkout credentials permit `git-upload-pack` for the claimed repo only and reject pushes. |
| Sandbox controls | `internal/runner/engine`, `deploy/runner-config`, `deploy/ansible/roles/shithubd-runner` | Container defaults drop caps, run as uid 65534, use read-only rootfs, pids/memory/CPU limits, seccomp, and no-new-privileges. |
| Network egress controls | `deploy/runner-config/dnsmasq.conf.j2`, `deploy/runner-config/firewall.sh.j2` | Runner role closes direct-IP egress by combining an allowlist resolver with ipset firewall rules. |
| Log safety | `internal/runner/scrub`, server-side runner log path, `shithub_actions_log_scrub_replacements_total` | Runner scrubs before send; server re-scrubs claim-time mask values and handles chunk-boundary secrets. |
| Lifecycle controls | `internal/actions/lifecycle`, repo Actions handlers, runner cancel-check | Cancel, re-run, timeout, concurrency, and retention paths are covered by focused tests and runbooks. |
| Observability | `deploy/monitoring/grafana/dashboards/actions.json`, `deploy/monitoring/prometheus/rules.yml` | Dashboard and alerts cover stale runners, queue depth, p99 regression, and scrubber health. |
| Operator docs | `docs/internal/runbooks/actions.md`, `docs/internal/runbooks/runner-deploy.md` | A fresh operator has register, deploy, smoke, emergency cancel, load, and incident procedures. |

## Pre-GA audit checklist

Run the local static packet first:

```sh
make audit-actions-ga
```

Then run focused tests:

```sh
go test -trimpath ./internal/actions/... ./internal/auth/runnerjwt ./internal/runner/... ./internal/web/handlers/api ./internal/web/handlers/repo ./internal/web/handlers/githttp
```

For a production-like runner host, manually verify:

1. Register a new runner with `self-hosted,linux,ubuntu-latest,x64`.
2. Trigger `.shithub/workflows/checkout-canary.yml` on trunk.
3. Confirm the run appears in the Actions tab and the check run completes.
4. Confirm step logs stream while the job is running and finalize to object
   storage after completion.
5. Run the sandbox smoke from `docs/internal/runbooks/runner-deploy.md`.
6. Re-test the S41e network fix: direct-IP egress and workflow-supplied
   resolvers must be blocked by the runner bridge firewall.
7. Echo a controlled test secret and confirm logs show `***`, not plaintext.
8. Reuse a consumed job JWT and confirm the API returns 401.
9. Run `bench/k6/actions-load.js` against a pre-seeded queue.
10. Verify the Grafana Actions dashboard is populated and Prometheus alert
    expressions parse in the monitoring stack.

## Accepted S41h deferrals

These are not S41h GA blockers because they were explicitly parked from v1 or
depend on the post-GA Nix/tooling work:

- Full marketplace-action compatibility.
- `actions/setup-go`, `golangci-lint-action`, cache actions, Docker actions,
  and composite actions.
- Matrix builds and reusable workflows.
- Hosted runner image provisioning by workflow authors. `runs-on` is a label
  selector; operators own the backing image.
- Full project CI migration from `.github/workflows/ci.yml` to
  `.shithub/workflows/ci.yml`.

## Next sprint hook

S41i should close the toolchain gap without weakening v1's security boundary:

- keep marketplace `uses:` rejected;
- provide a reproducible runner image or Nix engine path that can run `make ci`
  from first-party `run:` steps;
- keep deploy secrets out of untrusted PR workflows.
