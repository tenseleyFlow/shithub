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

Defaults:

- pool: `shared-linux`
- project: `shithub-prod`
- region: `sfo3`
- size: `s-2vcpu-4gb`
- image: `ubuntu-24-04-x64`
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
