# DigitalOcean runner pool helpers

These scripts are the S41j operator path for creating shithub Actions runner
hosts without using the DigitalOcean dashboard.

They create only infrastructure. They do not generate, store, or print runner
registration tokens.

## Provision a pool

```sh
SSH_KEY_NAME=macbook-pro \
SSH_ALLOWED_CIDRS=203.0.113.4/32 \
./deploy/doctl/provision-actions-runner-pool.sh --dry-run

SSH_KEY_NAME=macbook-pro \
SSH_ALLOWED_CIDRS=203.0.113.4/32 \
./deploy/doctl/provision-actions-runner-pool.sh
```

To converge the controlled-dogfood pool to three one-slot runners:

```sh
SSH_KEY_NAME=macbook-pro \
SSH_ALLOWED_CIDRS=203.0.113.4/32 \
COUNT=3 \
./deploy/doctl/provision-actions-runner-pool.sh --dry-run

SSH_KEY_NAME=macbook-pro \
SSH_ALLOWED_CIDRS=203.0.113.4/32 \
COUNT=3 \
./deploy/doctl/provision-actions-runner-pool.sh
```

The non-dry-run command creates paid DigitalOcean droplets. Use an explicit
operator approval checkpoint before running it from an automation session.

Defaults:

- pool: `shared-linux`
- project: `shithub-prod`
- region: `sfo3`
- size: `s-2vcpu-4gb`
- image: `ubuntu-24-04-x64`
- count: `1`
- tag: `shithub-actions-runner`
- cloud-init: `deploy/doctl/actions-runner-cloud-init.yaml`

`SSH_ALLOWED_CIDRS` must be one or more operator/VPN CIDRs. The provisioner
refuses `0.0.0.0/0` and `::/0` for SSH.

## Generate inventory

```sh
./deploy/doctl/generate-actions-runner-inventory.sh \
  --output deploy/ansible/inventory/actions-runners
```

Replace the generated token placeholders with per-host values from
`shithubd admin runner register`, preferably through ansible-vault or host_vars.
Generate one token per runner host:

```sh
shithubd admin runner register \
  --name actions-runner-1 \
  --labels self-hosted,linux,ubuntu-latest,x64 \
  --capacity 1 \
  --output json
```

Store the returned `token` in inventory/vault, not in shell history. Rotate by
registering a replacement token, deploying it to the host, confirming heartbeat,
then revoking the old runner token.
Use `--expires-in` only when that rotation is automated before the token
expires.

Then run:

```sh
make build
cd deploy/ansible
ansible-playbook -i inventory/actions-runners site.yml -t shithubd-runner
```

## Destroy a test pool

List runner droplets:

```sh
doctl compute droplet list --tag-name shithub-actions-runner
```

Delete specific test droplets by ID or name only after draining/revoking the
runner in shithub.
