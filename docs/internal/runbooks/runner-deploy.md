# Actions runner deploy runbook

This runbook owns the S41d deployment path for `shithubd-runner`: the
Nix-built default image, systemd unit, and Ansible role. The smoke flow
for an already-installed runner lives in [actions-runner.md](./actions-runner.md).

## Prereqs

- The app database is migrated through S41d and the web API has
  `auth.totp_key_b64` configured so job JWTs can be minted.
- Docker is installed on the runner host and the `docker` group exists.
  The runner process needs Docker socket access; treat the host itself
  as trusted even though individual step containers are sandboxed.
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
shithub_runner_seccomp_profile=/etc/shithubd-runner/seccomp.json
shithub_runner_container_user=65534:65534
shithub_runner_pids_limit=512
```

The role writes non-secret config to
`/etc/shithubd-runner/config.toml` and the registration token to
`/etc/shithubd-runner/runner.env` with mode `0600`.
Keep `shithub_runner_workspace_root` under `/var/lib/shithubd-runner`;
the systemd unit grants runner writes only to that subtree.

`shithub_runner_network_allowlist` defaults to GitHub source/archive
hosts plus Docker Hub registry hosts. Override it when a runner must
fetch from an internal package registry. The role creates the
`shithub-actions` Docker bridge at `172.30.0.1/24`, runs dnsmasq on
that bridge, and sets `engine.dns_servers` to the bridge resolver by
default.

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
- creates the dedicated Actions Docker network and bridge
- renders `/etc/dnsmasq.d/shithubd-runner.conf` from the network
  allowlist and starts dnsmasq bound to the Actions bridge
- installs `shithub-runner-firewall.service`, which rejects direct-IP
  egress from step containers unless dnsmasq populated the destination
  in the allowlist ipset
- installs the pinned seccomp profile at
  `/etc/shithubd-runner/seccomp.json`
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

Sandbox smoke checks:

```yaml
name: sandbox-smoke
on: push
jobs:
  smoke:
    runs-on: ubuntu-latest
    steps:
      - run: id -u
      - run: test "$(id -u)" = "65534"
      - run: if mkdir /etc/shithub-smoke 2>/dev/null; then exit 1; fi
      - run: if mount -t tmpfs tmpfs /mnt 2>/dev/null; then exit 1; fi
```

Expected state:

- the UID check prints `65534`
- a workflow-level request for `permissions: {shithub-runner-root: write}`
  still runs as `65534`; root opt-in is disabled in the shipped runner
  config until a trusted-workflow policy exists
- writing under `/etc` fails because the root filesystem is read-only
- `mount` fails because the container does not have `CAP_SYS_ADMIN`
- step logs and systemd journal include the configured image, network,
  CPU/memory limits, PID limit, container user, and seccomp profile

## Network Allowlist

The runner config carries two separate network controls:

- `runner.network_allowlist`: the host patterns allowed by the
  operator's DNS allowlist resolver.
- `engine.dns_servers`: DNS servers passed to each step container with
  Docker `--dns`.

For a single-host deployment, create a dedicated Docker bridge for
Actions jobs, run dnsmasq bound to that bridge, and set
`shithub_runner_dns_servers` to the bridge address of that resolver.
The Ansible role now does this by default. The rendered dnsmasq config
has no default upstream resolver; names not matching the allowlist fail
DNS resolution.

The firewall service closes the direct-IP bypass: containers on the
Actions subnet may send DNS only to the bridge resolver, and other
egress is allowed only when the destination IP is present in the
dnsmasq-populated ipset. Keep the runner on a separate host from web
and database services.

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
