# Deployment

This is the operator's guide to taking a fresh box from "Ubuntu 24.04
with sshd" to "running shithubd in production." It is opinionated:
DigitalOcean for compute, DigitalOcean Spaces for object storage,
Postgres on a dedicated droplet, Caddy as the edge, WireGuard for the
monitoring mesh. If you're running on something else, the Ansible
roles are the source of truth — read them.

## Topology

```
                +----------------------------+
   public --->  |  Caddy (TLS, rate limits)  |  :443
                +----------------------------+
                       |  127.0.0.1:8080
                +----------------------------+
                |  shithubd web (systemd)    |
                |  shithubd worker (systemd) |
                |  shithubd cron  (timer)    |
                +----------------------------+
                       |
                +-----------+         +-----------------+
                | Postgres  |         | Spaces (S3)     |
                +-----------+         | - WAL archive   |
                                      | - daily dumps   |
                                      | - LFS / blobs   |
                                      +-----------------+
                       \                    /
                        \      WireGuard mesh (10.50.0.0/24)
                         \________ ________/
                                  |
                          +----------------+
                          |  Monitoring    |
                          |  Prom/Loki/AM  |
                          |  Grafana       |
                          +----------------+
```

The monitoring host is *not* on the public internet. App processes
listen on `127.0.0.1` and on the wg0 mesh interface only; nothing
about the metrics port is reachable from outside the mesh.

## One-time bootstrap

1. **Provision the droplets.** Three for staging is enough (web,
   db, monitoring). Production starts at five (2× web, db, backup,
   monitoring) and grows the web tier first.
2. **Get sshd public-key login working** for the operator user. The
   Ansible base role narrows it from there.
3. **Populate `deploy/ansible/inventory/<env>`** by copying
   `inventory/staging.example`. The variables marked with `# REQUIRED`
   come from the operator's secret store (Bitwarden, 1Password, etc.).
   Do **not** commit a real inventory file.
4. **Bootstrap-admin user.** After the first deploy, ssh to the web
   host and run `shithubd admin bootstrap-admin --email you@…`. That
   gives you the site-admin bit; subsequent admin grants happen
   through `/admin/users/{id}`.

## Deploying

The Makefile wraps the playbook so the human commands are short:

```sh
# dry-run against staging (default inventory)
make deploy-check

# apply against staging
make deploy

# apply against production
ANSIBLE_INVENTORY=production make deploy

# only the app, not the edge or db
ANSIBLE_TAGS=app make deploy

# only one host (e.g. canary)
ANSIBLE_LIMIT=web-02 make deploy
```

The playbook is idempotent — run it twice in a row and the second
run should report `ok=N changed=0`. If the second run reports any
changes, that's a config drift bug; investigate before continuing.

## What the playbook does

In rough order:

- **base** — apt baseline, ufw default-deny, fail2ban, system users
  (`shithub`, `shithub-ssh`), data root at `/data`.
- **postgres** (`tags: [db]`) — installs PG16, initdb on `/data/pgdata`,
  applies our `postgresql.conf`/`pg_hba.conf`, wires the WAL archive
  command, creates the `shithub` and `shithub_hook` roles with
  exact-grant permissions.
- **shithubd** (`tags: [app]`) — copies the binary into
  `/usr/local/bin`, drops env files into `/etc/shithub/`, installs
  the three systemd units, restarts on change. The `web.service`
  ExecStartPre runs `shithubd migrate up` so a deploy with new
  migrations is one command.
- **caddy** (`tags: [edge]`) — installs Caddy + the templated
  `Caddyfile`. Auto-TLS via Let's Encrypt staging until the operator
  flips a vars flag; production after that.
- **wireguard** (`tags: [net]`) — peers each host into the mesh.
- **backup** (`tags: [backup]`) — installs the daily backup timer
  on the db host and the cross-region sync timer on the backup host.
- **monitoring-client** (`tags: [monitoring]`) — node-exporter +
  promtail on every host pointing at the monitoring host.

The monitoring host itself is provisioned by a separate Ansible play
that lives outside this repo (it depends on operator-specific TLS
material). The configs in `deploy/monitoring/` are the source of
truth for *what* runs there.

## Backups

Two layers, both mandatory:

1. **WAL archiving** (`deploy/postgres/archive_command.sh`) ships
   every WAL segment to `spaces-prod:shithub-wal` in real time.
   Postgres won't recycle a segment until the script reports success,
   so a failing archiver fills the disk — alert on
   `pg_stat_archiver.failed_count > 0`.
2. **Daily logical** (`deploy/postgres/backup-daily.sh`) takes a
   `pg_dump --format=custom` once per day and ships it to
   `spaces-prod:shithub-backups/daily/YYYY/MM/DD/`. Keeps the last
   7 locally for fast recovery.

Cross-region copy (`deploy/spaces/sync-cross-region.sh`) mirrors
both buckets to a second region for DR. Lifecycle in
`deploy/spaces/lifecycle.json` prunes WAL after 30 days and dumps
after 90.

The recovery target is **PITR within 30 days, full restore within
1 hour**. We verify this every quarter with the restore drill —
see `runbooks/restore.md`.

## Rollback

The deploy is a single binary + an env file + a systemd unit; rolling
back is "redeploy the previous binary." Two paths:

- **Tag rollback (preferred)** — `git checkout v<previous>` and
  `make deploy`. Migrations are forward-only by design; if the new
  release added a migration, you need to either accept that the
  rollback leaves the schema ahead, or apply the migration's matching
  `down` first (`shithubd migrate down`). Check `runbooks/rollback.md`
  before touching migrations.
- **Hotfix on a branch** — branch from the rolled-back tag, fix,
  cut a new release. Don't force-push tags.

## What goes where

| Concern                       | File                                           |
|-------------------------------|------------------------------------------------|
| Provisioning entrypoint       | `deploy/ansible/site.yml`                      |
| Per-environment vars          | `deploy/ansible/inventory/<env>`               |
| App systemd units             | `deploy/systemd/`                              |
| Edge config                   | `deploy/Caddyfile.j2`                          |
| sshd (incl. AKC for git)      | `deploy/sshd_config.j2`                        |
| Postgres scripts              | `deploy/postgres/`                             |
| Spaces lifecycle + DR         | `deploy/spaces/`                               |
| WireGuard mesh                | `deploy/wireguard/wg0.conf.j2`                 |
| Monitoring configs            | `deploy/monitoring/`                           |
| Restore drill                 | `deploy/restore-drill/`                        |
| Operator runbooks             | `docs/internal/runbooks/`                      |
