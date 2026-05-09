# Deploying with Ansible

The reference install path is the Ansible playbook in
`deploy/ansible/`. From a fresh Ubuntu 24.04 box the workflow is:
inventory → `make deploy` → bootstrap an admin → done.

## 1. Inventory

Copy the example inventory:

```sh
cp deploy/ansible/inventory/staging.example deploy/ansible/inventory/staging
$EDITOR deploy/ansible/inventory/staging
```

Variables marked `# REQUIRED` in the example come from your
secret store (Bitwarden, 1Password, ansible-vault). **Do not**
commit a real inventory file. The repo's `.gitignore` excludes
the bare names `staging` and `production` to make this obvious.

Critical variables:

- `db_password` — Postgres password for the `shithub` role.
- `hook_password` — Postgres password for the `shithub_hook`
  role.
- `session_key` — base64 32-byte AEAD key. Generate with
  `openssl rand -base64 32`.
- `totp_key` — base64 32-byte AEAD key for at-rest TOTP secrets.
- `s3_*` — bucket name, region, credentials.
- `email_*` — Postmark token or SMTP creds.
- `wireguard_*` — peer keys for the monitoring mesh.

## 2. Dry-run

```sh
make deploy-check ANSIBLE_INVENTORY=staging
```

Reports every change Ansible would make without making it. Read
the diff before continuing.

## 3. Apply

```sh
make deploy ANSIBLE_INVENTORY=staging
```

The playbook is idempotent; a second run should report
`changed=0`. If it doesn't, that's a config drift bug — open an
issue.

The roles in order:

- **base** — apt baseline, ufw default-deny, system users (`shithub`,
  `shithub-ssh`), data root.
- **postgres** — installs PG16, initdb's `/data/pgdata`, applies
  our `postgresql.conf` and `pg_hba.conf`, wires the WAL archive
  command, creates `shithub` and `shithub_hook` roles with
  exact-grant permissions.
- **shithubd** — copies the binary into `/usr/local/bin`, drops
  env files into `/etc/shithub/`, installs the three systemd
  units. The web service's `ExecStartPre=` runs `shithubd migrate
  up` so deploys with new schema are one command.
- **caddy** — installs Caddy + the templated `Caddyfile`. Auto-
  TLS via Let's Encrypt staging until you flip `caddy_use_acme_
  staging=false`.
- **wireguard** — peers each host into the 10.50.0.0/24 mesh.
- **backup** — installs the daily backup timer on the db host
  and the cross-region sync timer on the backup host.
- **monitoring-client** — node-exporter + promtail on every
  host pointing at the monitoring host.

## 4. Bootstrap the first admin

The first deploy creates no users. SSH to the web host and run:

```sh
sudo -u shithub /usr/local/bin/shithubd admin bootstrap-admin --email you@example.com
```

The CLI creates a user (or grants site-admin to an existing one)
and prints a one-time password-reset link. Open it in a browser,
set a password, enable 2FA. Subsequent admin grants happen
through `/admin/users/{id}`.

## 5. Smoke

- `https://shithub.sh/` — Caddy serves the home page.
- `https://shithub.sh/-/health` — returns `200 OK` with the
  build version.
- Sign in as the bootstrap admin. Create a test repo. Push to it.
- `https://shithub.sh/admin/` — admin dashboard renders.

## 6. Production

Same procedure with the production inventory:

```sh
make deploy ANSIBLE_INVENTORY=production
```

For larger deploys, target a single host first:

```sh
make deploy ANSIBLE_INVENTORY=production ANSIBLE_LIMIT=web-02
```

## Postgres

The `postgres` role does the heavy lifting; nothing operator-
visible should be needed. Specific things to be aware of:

- **Archive command.** `deploy/postgres/archive_command.sh` ships
  every WAL segment to `spaces-prod:shithub-wal`. Postgres
  refuses to recycle a segment until the script reports success;
  a failing archiver therefore fills the disk. Alert on
  `pg_stat_archiver.failed_count > 0` (the default rule does).
- **Hook role.** A separate Postgres role `shithub_hook` is used
  by the `shithubd hook …` subcommands. It has minimal grants —
  see `deploy/postgres/hook-role-grants.sql`. If you add a new
  hook subcommand that touches a new table, update those grants.

## Caddy

The Caddyfile (`deploy/Caddyfile.j2`) reverse-proxies
`127.0.0.1:8080`. Two special path patterns:

- `/_/<owner>/<repo>.git/(info/refs|git-(upload|receive)-pack)`
  uses a 30-minute timeout to accommodate large pushes / clones.
- Static assets under `/static/` get aggressive cache headers.

Access logs are JSON to `/var/log/caddy/access.log`; promtail
ships them to Loki.

## sshd

The provided `sshd_config.j2` is conservative: PasswordAuthentication
off, PubkeyAuthentication on, X11/agent/TCP forwarding off. The
`Match User git` block uses the AKC contract — see
[Authentication / SSH on the user side](../user/ssh.md).

## Rolling forward

```sh
git fetch --tags
git checkout v<new-version>
make deploy ANSIBLE_INVENTORY=production
```

That's it. The `migrate up` is part of the unit's startup
preflight; if a migration fails, the unit stays in `activating`
and the journal has the error. See
[Upgrades](./upgrades.md) for the major-version checklist.
