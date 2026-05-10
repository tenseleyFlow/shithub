# Upgrade

Routine release deploys. The deploy is one binary swap + a systemd
restart; the only place upgrades get exciting is around DB migrations
and the occasional config schema change.

## Standard release

Pushes to `trunk` auto-deploy to production via the `deploy` GitHub
Actions workflow once `ci` succeeds. The workflow SSHes to the app
droplet and runs `deploy/redeploy.sh`, which fetches trunk, rebuilds
the binary in place, runs `migrate up`, and restarts the web + worker
units. There is no canary tier today (see "Canary" below).

To redeploy current trunk without a push (e.g., after editing env
files on the droplet), trigger the `deploy` workflow manually:
`gh workflow run deploy.yml --ref trunk`. To deploy by hand from a
console:

```sh
ssh root@shithub.sh 'bash /root/src/shithub/deploy/redeploy.sh'
```

For tagged releases on a staging-then-prod path (once we have a
staging tier):

```sh
# from a clean checkout of the release tag
git fetch --tags
git checkout v<version>
make deploy-check ANSIBLE_INVENTORY=staging
make deploy ANSIBLE_INVENTORY=staging
# ... canary period ...
make deploy ANSIBLE_INVENTORY=production
```

### GitHub Actions secrets

The `deploy` workflow needs three repo secrets (Settings → Secrets
and variables → Actions, in the `production` environment):

- `DEPLOY_HOST` — `shithub.sh` (or the app droplet's public IPv4)
- `DEPLOY_USER` — `root`
- `DEPLOY_SSH_KEY` — private half of an ed25519 key whose public half
  is in `/root/.ssh/authorized_keys` on the app droplet
- `DEPLOY_KNOWN_HOSTS` — output of `ssh-keyscan shithub.sh` on a
  trusted host, pinning the host key so the runner won't TOFU-trust
  a hijacked DNS answer

Generate a dedicated deploy key (don't reuse the operator's laptop
key):

```sh
ssh-keygen -t ed25519 -C 'gh-actions-deploy' -f ./gh-deploy -N ''
ssh-copy-id -i ./gh-deploy.pub root@shithub.sh
ssh-keyscan shithub.sh > known_hosts.txt
# Paste ./gh-deploy            → DEPLOY_SSH_KEY
# Paste known_hosts.txt        → DEPLOY_KNOWN_HOSTS
# Then: rm gh-deploy gh-deploy.pub known_hosts.txt
```

`shithubd migrate up` runs as the web service's ExecStartPre, so
the binary that needs the new schema is also the one that applies
it. Order on each host: ExecStartPre runs migrations → web starts
on the new schema.

If a migration is long (>30s), call it out in the release notes
and time the deploy outside peak hours. The web service hangs in
"activating" until ExecStartPre finishes.

## Canary

We deploy to staging first, watch for 30 min in Grafana. Things to
look at:

- p95 latency on the top routes (`shithubd-overview` dashboard).
- DB call rate — a 10× jump usually means a regressed N+1.
- Job queue depth — a stuck migration reflects here.
- Error logs in Loki: `{service="shithubd"} |~ "panic|ERROR"`.

If anything looks off, **do not** promote to production. Rollback
on staging is cheap; rollback on production is loud.

## Major version (database)

If the release notes flag a major schema change:

1. Take a manual `pg_dump` immediately before the deploy:
   `sudo -u postgres /usr/local/bin/shithub-backup-daily`.
2. Confirm it landed in Spaces.
3. Deploy to staging, run `make restore-drill` against the
   *post-deploy* dump to confirm the new schema restores cleanly.
4. Then production.

## Config schema changes

When a release adds a required env var, the binary refuses to start
and complains in the journal. Update `deploy/ansible/roles/shithubd/
templates/web.env.j2` (and `worker.env.j2`), bump the inventory
vars, redeploy. There's no separate migration step for env files.
