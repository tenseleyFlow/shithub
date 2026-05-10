# AIDE — file-integrity monitoring

AIDE (Advanced Intrusion Detection Environment) hashes a chosen set
of system files at install time and re-checks them nightly. We use
it to catch the post-compromise persistence pattern — someone with
root replaces `/usr/local/bin/shithubd`, drops a systemd unit in
`/etc/systemd/system/`, modifies `/root/.ssh/authorized_keys`, etc.
The daily check produces no output when nothing's changed and a
loud journal entry when something has.

## Where alerts surface

```sh
journalctl -t shithub-aide -n 200 --no-pager
tail -100 /var/log/shithub/aide.log
```

The wrapper at `/usr/local/bin/shithub-aide-check` writes both:

- `/var/log/shithub/aide.log` — append-only, persists across reboots.
- `journalctl -t shithub-aide` — structured, queryable, ships
  with whatever log shipper we add later.

A `/var/run/shithub-aide.last-clean` heartbeat file is updated on
every clean run so the operator can confirm the cron actually
fires:

```sh
stat /var/run/shithub-aide.last-clean
# Modify: 2026-05-10 03:30:12 +0000 UTC   ← yesterday's clean run
```

Email delivery is **not yet wired**. The droplet has no MTA and
the project's outbound SMTP (Postmark) is approval-gated. Once
Postmark is approved, swap the `systemd-cat` call in the wrapper
for a `curl POST https://api.postmarkapp.com/email …` invocation
using the existing `SHITHUB_AUTH__POSTMARK__SERVER_TOKEN` (read
the env file from inside the wrapper).

## When alerts fire

1. Look at the journal entry. Each diff line is one of:
   - `f` — file content changed (size, mtime, hash)
   - `+` / `-` — file added / removed
   - `d` — directory metadata changed
2. **Match the diff against an authorized change**:
   - `apt` / `unattended-upgrades` ran → expect changes under
     `/usr/lib/`, `/usr/sbin/`, `/etc/apt/`. Cross-check against
     `journalctl -u unattended-upgrades` for the same timeframe.
   - A deploy ran → expect `/usr/local/bin/shithubd` to change.
     Cross-check the SHA against `gh run list --workflow=deploy.yml`.
   - A manual config edit → match against the operator's notes.
3. **No authorized change matches** → treat as an incident. Open
   `runbooks/incidents.md`. Don't re-baseline AIDE until the
   investigation closes.

## Re-baselining after an authorized change

Whenever you make an intentional change to a watched path (apt
upgrade, manual config edit, ansible-driven config change), the
next nightly run will flag it. Re-baseline once the change is
confirmed-good:

```sh
sudo aideinit -y -f
sudo mv /var/lib/aide/aide.db.new.gz /var/lib/aide/aide.db.gz
sudo rm -f /var/lib/aide/.config-changed
```

The `-y` is "answer yes to all prompts," `-f` is "overwrite an
existing new database." Run takes 1–3 minutes on a 4 GB droplet.

## Re-baselining after an Ansible config change

When `deploy/ansible/roles/base/files/aide-shithub.conf` is edited
and the playbook re-runs, the `rebuild aide database` handler
drops `/var/lib/aide/.config-changed`. Re-baseline as above to
clear it.

## Disabling temporarily

If you're about to do a large planned change (OS upgrade, big
ansible re-run) and don't want a flood of alerts:

```sh
# Disable for the next 24h
sudo systemctl stop cron     # blunt; you may prefer to mv just the cron entry
# ... make changes ...
sudo aideinit -y -f && \
sudo mv /var/lib/aide/aide.db.new.gz /var/lib/aide/aide.db.gz
sudo systemctl start cron
```

Or, surgically, comment out the `30 3 * * * /usr/local/bin/shithub-aide-check`
line in `crontab -l`. Re-baseline + re-enable when done.

## What's watched, what isn't

The default Debian config (`/etc/aide/aide.conf` and the snippets
in `/etc/aide/aide.conf.d/`) covers `/etc`, `/bin`, `/sbin`,
`/usr/{bin,sbin,lib,libexec,local}`, `/root`, `/boot`, `/lib*`.
Our exclusions (`/etc/aide/aide.conf.d/99_shithub_exclude`):

| Path | Why excluded |
|---|---|
| `/data` | Repo data root — write-heavy by design |
| `/var/lib/postgresql` | Postgres rewrites these constantly |
| `/var/lib/shithub*` | Application state |
| `/var/lib/caddy`, `/var/log/caddy` | Cert renewals + access log churn |
| `/var/log/shithub` | App logs (incl. our own aide.log) |
| `/root/src/shithub` | Source tree fetched by every deploy |
| `/usr/local/share/shithub` | Restore-drill scratch |
| `/var/backups/shithub` | Nightly pg_dump |
| `/var/lib/aide` | AIDE's own DB |
| `/tmp/shithubd-new` | Deploy step's binary swap path |

If you add a new system path that legitimately churns, add it
here, commit, re-run ansible, then re-baseline.
