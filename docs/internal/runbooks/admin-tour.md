# Admin tour

The site-admin surface — what's available, where to find it, and what
each thing does. Audience: the first operator (you) signing in to a
fresh production instance.

## Becoming an admin

The first user is not automatically an admin; the site treats the
schema and the human as separate concerns so a self-signup never
escalates itself. Promote yourself once via the CLI:

```sh
ssh root@shithub.sh
sudo -u shithub /usr/local/bin/shithubd admin bootstrap-admin <username>
```

`bootstrap-admin` records an audit row with `actor_id = 0` so the
"who promoted whom" history is never blank. After that, additional
admins are toggled from the UI — see `/admin/users/<id>`.

To stop being an admin, the same UI toggle works (or a peer admin can
revoke you). There is intentionally no CLI demote: revocation should
leave an audit row by an authenticated actor.

## Web surface

All admin routes live under `/admin/*` and are gated by
`RequireSiteAdmin`. Non-admins get a 404, not a 403 — existence is
not leaked.

### Dashboard — `/admin`
Counts: users, repos, orgs, jobs, admins. First glance to confirm the
instance is alive and to spot anomalies (e.g., 10× job count overnight).

### Users — `/admin/users` and `/admin/users/{id}`
Lists users with suspension state and admin flag. The detail page has
buttons for:
- Suspend / unsuspend (writes audit, sends no email — quiet sanction).
- Toggle site-admin (writes audit, irrevocable from CLI).
- Force a password-reset link (1-hour token, mailed to primary email;
  also surfaces in the journal if email backend is `stdout`).

### Repos — `/admin/repos` and `/admin/repos/{id}`
Filterable: deleted, archived. The detail page can force-archive or
force-delete a repo. Hard-delete is two-step: first soft-delete (lands
in the restore queue with a TTL), then purge after the TTL or via the
restore-list page.

### Jobs — `/admin/jobs` and `/admin/jobs/{id}`
The worker queue. Filter by kind (`email_send`, `webhook_deliver`,
`repo_archive_purge`, …) and status (`queued`, `running`, `failed`,
`succeeded`). Detail page shows the JSON payload and the last error,
with buttons to **Retry** (re-enqueue) and **Discard** (mark dead).
First place to look when "the email never arrived" or "the webhook
didn't fire."

### Audit — `/admin/audit`
Filterable history of security-relevant events: admin actions, login
successes/failures, 2FA changes, key revocations, impersonation. All
the filter dimensions live in the query string (`actor`, `action`,
`target_type`, `target_id`, `since`, `until`). Bookmarkable.

### System — `/admin/system`
Runtime introspection: shithubd version + commit, Go runtime, DB
connection-pool stats, repo-disk-usage rollup. Useful for "is this
the version I just deployed?" — compare commit hash against
`git log -1 origin/trunk`.

### Email — `/admin/email`
Outbound transactional email queue and last-N delivery results. If a
verification or reset mail is missing, this is the second place to
check (after `/admin/jobs` filtered to `email_send`).

### Impersonation — `/admin/impersonate/{id}`
Open a session as another user in **read-only** mode. All write
endpoints reject with 403 from an impersonated session unless you
explicitly switch to `/admin/impersonate/write-mode`. Both the start
and the writable-promotion are audited.

End impersonation with `/admin/impersonate/stop` (or via the banner
that appears at the top of every page during an impersonation).

## CLI subcommands

Run as the system user that owns the binary's environment file. The
canonical invocation is `sudo -u shithub /usr/local/bin/shithubd
admin <subcommand>`; the binary reads `/etc/shithub/web.env` only
when the systemd unit launches it, so for ad-hoc admin commands you
either source that file first or rely on `shithubd`'s own env-only
config (it'll print which knob is missing if so).

| subcommand | purpose |
|---|---|
| `bootstrap-admin <username>` | Promote first user (chicken-and-egg). |
| `reset-password <username>` | Issue a 1-hour password-reset link. |
| `clear-2fa <username>` | Wipe TOTP enrollment (support escape hatch). User is emailed. |
| `run-job <kind> [json-payload]` | Enqueue an arbitrary worker job. |
| `recompute <metric>` | Recompute denormalized counters (`star_count`, `fork_count`). |

`reset-password` and `clear-2fa` both leave audit rows attributed to
`actor_id = 0` (system) so a recovered account always has an honest
trail.

## When to look where

| Symptom | First stop |
|---|---|
| User says "didn't get my email" | `/admin/jobs?kind=email_send&status=failed` then `/admin/email` |
| User locked out of 2FA | CLI `clear-2fa <user>` (then verify audit row) |
| Suspicious activity from one IP | `/admin/audit?actor=<id>` + `fail2ban-client status shithubd-auth` on the droplet |
| "Did this PR ship?" | `/admin/system` for the running commit; compare to `git rev-parse origin/trunk` |
| Worker queue piling up | `/admin/jobs?status=queued` — sort by age, retry or discard the oldest |
| Repo went missing | `/admin/repos?deleted=1` — soft-deleted lives in the restore queue |
| Need to peek at a user's view | `/admin/impersonate/<id>` (read-only by default) |

## What the admin surface deliberately does NOT have

- **Bulk user actions.** No "suspend N users matching a filter."
  Anything destructive at scale should be a thought-out script, not
  one click.
- **API endpoints.** Admin is HTML-only. Token-based admin actions
  would let a leaked session token compromise the whole instance;
  cookie sessions plus the per-action audit row keep the blast
  radius bounded.
- **Direct DB editing.** If you find yourself wanting it, the CLI
  doesn't have it for a reason — write a migration or a one-off
  job kind, then enqueue with `admin run-job`.
