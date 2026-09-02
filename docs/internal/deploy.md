# Deployment

This is the operator's guide to taking a fresh box from "Debian/Ubuntu
with sshd" to "running shithubd in production." It is opinionated:
DigitalOcean for compute, DigitalOcean Spaces for object storage,
Caddy as the edge, Postgres 16, Grafana Alloy pushing metrics to
Grafana Cloud. If you're running on something else, the Ansible roles
are the source of truth — read them.

Two topologies are described below. The **single-box reference
deployment** is what shithub.sh actually runs and what the Ansible
roles are tuned for. The **multi-host design** is the aspirational
shape we'd grow into; nothing in it is deployed today.

## Single-box reference deployment (what shithub.sh runs)

```
                +----------------------------+
   public --->  |  Caddy (TLS, rate limits)  |  :443
                +----------------------------+
                       |  127.0.0.1:8080
   +--------------------------------------------------------+
   |  droplet `shithub-app`  — 2 vCPU / 3.9 GB / 4 GB swap   |
   |                                                        |
   |   shithubd web (systemd)    ---.                       |
   |   shithubd worker (systemd)  ---+--> Postgres 16       |
   |   shithubd cron (timer, 05:15) -'    (localhost:5432,  |
   |   Caddy                               peer + SCRAM)    |
   |   Grafana Alloy + node_exporter                        |
   |   root crontab: pg_dump, rclone DR sync, AIDE,         |
   |                 WAL-archive verify                     |
   +--------------------------------------------------------+
          |                                    |
          v                                    v
   Spaces (S3)                          Grafana Cloud
   - WAL archive                        (Prometheus/Mimir,
   - daily dumps                         remote_write, push-only)
   - LFS / blobs / Actions logs

   3 × runner droplets — outbound HTTPS to the app box only;
   inbound SSH restricted by cloud firewall to the app box.
```

Everything is on one host. There is **no private network, no VPN, and
no separate database, backup, or monitoring host.** Postgres listens on
`localhost` only; `/metrics` is served on `127.0.0.1:8080` and is
reached only by the local Alloy process. Alloy `remote_write`s to
Grafana Cloud, so no inbound monitoring port exists at all.

### Memory budget

3.9 GB of RAM plus a 4 GB swapfile on `/data` (`vm.swappiness=10`).
Baseline figures are the measurements in the 2026-09-02 availability
sitrep; the ceilings are what the shipped units enforce.

| Component | Ceiling | Observed / expected |
|---|---|---|
| `shithubd-web` | `GOMEMLIMIT=1200MiB`, `MemoryHigh=1600M`, `MemoryMax=2000M`, `OOMScoreAdjust=-500` | 1.33 GB RSS before Phase 2; the eight duplicate template renderers (664 MB) are now one (41 MB), so steady-state target is < 600 MB |
| `shithubd-worker` | `MemoryHigh=768M`, `MemoryMax=1024M` (cgroup-wide, so forked `git` counts) | small; spikes with pack operations |
| `shithubd-cron` | inherits the worker-class footprint; runs 05:15 UTC | short-lived |
| Postgres 16 | none (not a cgroup we manage) | 447 MB aggregate at the Debian-default `shared_buffers=128MB`; ~600 MB if `postgresql.conf.j2` is ever applied (see `db.md`) |
| Grafana Alloy | none | 317 MB scraping an 11.7k-series `/metrics` (Phase 3 should cut the series count by an order of magnitude) |
| Caddy, node_exporter, sshd, journald | none | ~100 MB combined |
| `rclone` DR sync | 4×/day under `flock`, off the 03:00–05:00 window | 1.0–1.6 GB for ~28 min per run; this is the single largest transient |
| AIDE check | 06:00 UTC, one wrapper only | ~0.5 GB for ~12 min |

The failure mode this budget exists to prevent: baseline ~2.4 GB +
AIDE ~0.5 GB + rclone 1.0–1.6 GB exceeded 3.9 GB with no swap, and the
kernel picked the largest process — nine `global_oom` kills in the week
before 2026-09-02, alternating between `shithubd` and `rclone`.

### Observability on this box

- **Metrics.** node_exporter on `127.0.0.1:9100`, shithubd on
  `127.0.0.1:8080/metrics`. Grafana Alloy scrapes both and
  `remote_write`s to Grafana Cloud. Scrape job labels are `node` and
  `shithubd` — not the `shithubd-web` / `shithubd-worker` / `postgres`
  / `caddy` jobs the committed Prometheus config assumes.
- **The worker exposes no metrics endpoint.** `shithubd worker` never
  starts an HTTP listener, so every `shithub_worker_*` series exists in
  the binary and reaches nothing.
- **Logs.** journald only. There is **no log shipping** — no Promtail,
  no Loki, no Alloy logs pipeline. Reading logs means SSH +
  `journalctl`.
- **Alerting.** DigitalOcean droplet alerts and the uptime check
  (`runbooks/alerts.md`) plus whatever Grafana-managed alert rules the
  operator has provisioned in Grafana Cloud. **There is no
  Alertmanager and no local Prometheus**, so nothing in
  `deploy/monitoring/prometheus/rules.yml` can fire — see
  `deploy/monitoring/README.md`.

Details, queries and the operator setup flow:
`runbooks/observability.md`.

<!-- topology:aspirational-start -->

## Multi-host design (aspirational — not deployed)

Nothing in this section is running. It is the shape we would grow
into if one box stops being enough, and it is what
`deploy/ansible/roles/wireguard/`, `deploy/monitoring/` and the
`monitoring-host` language elsewhere in the tree assume. Treat every
reference to a mesh address, a monitoring host, Prometheus, Loki or
Alertmanager as a design note, not as production.

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

In that design the monitoring host is *not* on the public internet;
app processes listen on `127.0.0.1` and on the `wg0` mesh interface
only, and nothing about the metrics port is reachable from outside the
mesh. Adopting it means provisioning the mesh
(`deploy/ansible/roles/wireguard/`), standing up the monitoring host,
and repointing the scrape config — see `deploy/monitoring/README.md`
for the gap list.

<!-- topology:aspirational-end -->

## One-time bootstrap

1. **Provision the droplet.** One box runs everything (see the
   reference deployment above); 4 GB is the practical floor, 8 GB is
   comfortable. Actions runners are separate droplets with a cloud
   firewall allowing inbound SSH from the app box only. The
   multi-host split (2× web, db, backup, monitoring) is the
   aspirational shape, not the starting point.
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
  (`shithub`, `shithub-ssh`), data root at `/data`, a 4 GB swapfile
  at `/data/swapfile` with `vm.swappiness=10`, and AIDE
  file-integrity monitoring (nightly at 06:00 UTC; the packaged
  `dailyaidecheck.timer` is disabled so only our wrapper runs).
- **postgres** (`tags: [db]`) — installs PG16, initdb on `/data/pgdata`,
  applies our `postgresql.conf`/`pg_hba.conf`, wires the WAL archive
  command, creates the `shithub` and `shithub_hook` roles with
  exact-grant permissions.
- **shithubd** (`tags: [app]`) — copies the binary into
  `/usr/local/bin`, drops env files into `/etc/shithub/`, installs
  the three systemd units, restarts on change. The `web.service`
  ExecStartPre runs `shithubd migrate up` so a deploy with new
  migrations is one command. The units carry cgroup memory ceilings
  (`MemoryHigh`/`MemoryMax`, plus `GOMEMLIMIT` for web) — see the
  2026-09-02 availability sitrep for the sizing rationale.
- **caddy** (`tags: [edge]`) — installs Caddy + the templated
  `Caddyfile`. Auto-TLS via Let's Encrypt staging until the operator
  flips a vars flag; production after that.
- **backup** (`tags: [backup]`) — installs the daily `pg_dump` cron
  (03:17) and the 6-hourly cross-region Spaces sync (01/07/13/19:23,
  under `flock`). On the single-box deployment both land on the app
  host.
- **monitoring-client** (`tags: [monitoring]`) — `node_exporter` on
  `127.0.0.1:9100` plus Grafana Alloy, which scrapes node_exporter and
  shithubd's `/metrics` and `remote_write`s to Grafana Cloud. Metrics
  only; it ships no logs.
<!-- topology:aspirational-start -->

- **wireguard** (`tags: [net]`) — peers each host into the WireGuard
  mesh. Part of the aspirational multi-host design; **not run today**
  (a single-host inventory has no peers).

<!-- topology:aspirational-end -->

There is no monitoring host. The configs in `deploy/monitoring/` are
not deployed anywhere — see `deploy/monitoring/README.md`.

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
both buckets to a second region for DR, every 6 h (01/07/13/19:23
UTC) under a `flock` so runs cannot overlap. The WAL leg drops
`--fast-list` — buffering a 161k-object listing cost ~1 GB of RSS on
a 3.9 GB box. The full job schedule, and why the jobs are spaced the
way they are, is in `runbooks/backups.md`. Lifecycle in
`deploy/spaces/lifecycle.json` prunes WAL after 30 days and dumps
after 90. Actions log/artifact objects use the primary object bucket's
`actions/runs/` prefix; apply `deploy/spaces/actions-lifecycle.json`
with `deploy/cutover/apply-actions-lifecycle.sh` so provider-side blob
retention matches the `workflow:cleanup` database sweep.

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
| Metrics agent (deployed)      | `deploy/ansible/roles/monitoring-client/`      |
| Monitoring configs (undeployed)| `deploy/monitoring/` — see its `README.md`    |
| Mesh role (aspirational)      | see the multi-host section above               |
| Restore drill                 | `deploy/restore-drill/`                        |
| Operator runbooks             | `docs/internal/runbooks/`                      |
