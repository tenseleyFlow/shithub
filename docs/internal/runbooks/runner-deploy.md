# Actions runner deploy runbook

This runbook owns the S41d deployment path for `shithubd-runner`: the
Nix-built default image, systemd unit, and Ansible role. The smoke flow
for an already-installed runner lives in [actions-runner.md](./actions-runner.md).

## Prereqs

- The app database is migrated through S41d and the web API has
  `auth.totp_key_b64` configured so job JWTs can be minted.
- Docker is installed on the runner host and the `docker` group exists.
  S41e narrows the sandbox; S41d runner hosts must be treated as trusted.
- `bin/shithubd-runner` exists locally. `make build` builds both
  `bin/shithubd` and `bin/shithubd-runner` with the same version ldflags.
- The default image has been loaded or published. Build it with:

```sh
nix build ./deploy/runner-images#runnerImage
docker load < result
```

The committed `deploy/runner-images/flake.lock` pins the nixpkgs input.
Update it deliberately when changing the default image toolchain.

Publishing to GHCR is manual through `.github/workflows/runner-image.yml`
because forks may not control the upstream `ghcr.io/shithub` namespace.
Leave the workflow's `image` input blank to publish under the current
repository's package namespace, or set it explicitly for upstream
publishing.

## Register

Run this once from a host that can reach the production database config:

```sh
shithubd admin runner register \
  --name prod-runner-1 \
  --labels self-hosted,linux,ubuntu-latest \
  --capacity 1
```

Store the printed token in ansible-vault or the deployment secret store.
Only the token hash is stored in Postgres; the raw token cannot be
recovered later.

## Inventory

Enable the role explicitly. The default is disabled so ordinary app
deploys do not start a runner by accident.

```ini
[shithub:vars]
shithub_runner_enabled=true
shithub_runner_token=REPLACE_ME
shithub_runner_labels=self-hosted,linux,ubuntu-latest
shithub_runner_capacity=1
shithub_runner_default_image=ghcr.io/shithub/runner-nix:1.0
```

The role writes non-secret config to
`/etc/shithubd-runner/config.toml` and the registration token to
`/etc/shithubd-runner/runner.env` with mode `0600`.
Keep `shithub_runner_workspace_root` under `/var/lib/shithubd-runner`;
the systemd unit grants runner writes only to that subtree.

## Deploy

For the runner role only:

```sh
make build
cd deploy/ansible
ansible-playbook -i inventory/production site.yml -t shithubd-runner
```

The role:

- creates the `shithub-runner` system user and joins it to `docker`
- uploads `/usr/local/bin/shithubd-runner`
- renders `/etc/shithubd-runner/config.toml` and `runner.env`
- installs `deploy/systemd/shithubd-runner.service`
- pulls the configured runner image
- enables and starts `shithubd-runner`

## Verify

On the runner host:

```sh
systemctl status shithubd-runner
journalctl -u shithubd-runner -n 100 --no-pager
```

Then push a workflow with a simple `run:` step:

```yaml
name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: bash -c "echo hello && exit 0"
```

Expected state:

- a runner heartbeat claims the queued job within one idle poll interval
- the step emits SQL log chunks during execution
- `workflow:finalize_step` uploads
  `actions/runs/<run_id>/jobs/<job_id>/steps/<step_id>.log`
- the job and check run complete with conclusion `success`

Repeat with `exit 1`; the check should complete with conclusion
`failure`.

## Rollback

Stop the runner first so it does not claim new jobs:

```sh
systemctl stop shithubd-runner
systemctl disable shithubd-runner
```

If the binary itself is bad, copy a prior archived binary from
`/var/lib/shithubd-runner/binaries/` back to
`/usr/local/bin/shithubd-runner` and restart the unit. Jobs already
claimed by the stopped runner remain visible in the database; S41g adds
operator cancel/re-run controls.
