# Prerequisites

What you need before you start. The reference deployment targets
DigitalOcean + DigitalOcean Spaces; if you're on a different
provider, the shape is the same — substitute equivalents.

## Compute

Three machines for staging is comfortable; production starts at
five. The minimum useful sizes:

| Role         | vCPU | RAM  | Disk | Notes                                    |
|--------------|-----:|-----:|-----:|------------------------------------------|
| web          | 2    | 4 GB | 40 GB | Run two for HA.                         |
| worker       | 2    | 4 GB | 40 GB | Can colocate with web for small instances. |
| postgres     | 2    | 8 GB | 100 GB | Start at 100 GB; grow with disk usage.  |
| backup       | 1    | 2 GB | 80 GB | Idle most of the time; runs cron only.  |
| monitoring   | 2    | 4 GB | 60 GB | Prom + Loki + Alertmanager + Grafana.   |

Ubuntu 24.04 LTS is the supported OS. Other Debian derivatives
work but are not regularly tested.

## Storage

shithub stores three categories:

- **Bare repos** — on the web/worker hosts at `/data/repos`. Local
  filesystem; **not** in object storage. Sized to total push
  volume.
- **Webhook delivery bodies, LFS, attachments** — in an
  S3-compatible object store. The reference setup uses
  DigitalOcean Spaces; MinIO works for self-hosted equivalents.
- **Backups** — separate bucket. Cross-region mirror is strongly
  recommended; lifecycle policies prune old data.

## Database

Postgres 16 on a dedicated host. Earlier versions work in
development; production requires 16+ for the `pg_stat_statements`
features and JSON functions we rely on.

The `archive_command` ships WAL to object storage in real time
([deploy.md](./deploy.md#postgres)); a daily logical dump is taken
on top of that.

## Domain + TLS

You need:

- A domain you control (e.g. `shithub.example`).
- DNS records for the app (`shithub.example`) and the docs
  subdomain (`docs.shithub.example`).
- A TLS certificate. Caddy obtains and renews via Let's Encrypt
  automatically — no manual cert management — but the DNS records
  must point at your public IP first.

## Email

shithub sends transactional email (signup verification, password
reset, notifications). Three backends:

- `stdout` — prints to the journal. **Dev only.**
- `smtp` — talk to your own MTA or a SaaS that exposes SMTP.
- `postmark` — uses Postmark's HTTP API (the recommended
  production path; Postmark's deliverability is excellent for
  transactional mail).

Warm-up your sending domain before going live; cold IPs have
their first thousand emails treated as spam.

## SSH

The git server expects port 22 reachable from your users. The
sshd config (deploy/sshd_config.j2) restricts the `git` user to
the AKC-driven flow; the rest of sshd is operator-only.

## Object store credentials

Store both an access key for the runtime (read+write to the
bucket) and a separate set for backups + restore drill (read-only
on the runtime bucket; full access on the backup bucket).
Rotate quarterly.

## Time

NTP must be running; the rate-limit windows, TOTP, and audit-log
timestamps depend on monotonic, accurate time. Ubuntu's
`systemd-timesyncd` is enough.

## Skills you'll want

- Comfort with Ansible — the playbook is the install path.
- Postgres operational basics: pg_dump/pg_restore, monitoring,
  WAL.
- Reading systemd journal output (`journalctl -u …`).

If any of those are unfamiliar, allow extra time on your first
deploy and follow the runbooks closely.
