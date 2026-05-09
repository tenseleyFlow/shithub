# Troubleshooting

Common operator-visible failure modes and how to diagnose them.

## Caddy: certificate acquisition fails

**Symptom:** Caddy logs show `failed to obtain certificate` for
your domain; the site serves the staging cert (or no cert).

Most often:

- DNS doesn't yet point at the host. Verify with `dig +short
  shithub.example`.
- Port 80 is blocked. Let's Encrypt's HTTP-01 challenge needs
  port 80 reachable from the public internet (Caddy redirects
  to 443 *after* obtaining the cert). UFW must allow 80.
- Rate-limited by Let's Encrypt. If you've been retrying with
  `caddy_use_acme_staging=false` and failing, you can be locked
  out for a few hours. Switch to `caddy_use_acme_staging=true`,
  reload, then flip back when DNS/firewall is correct.

## sshd: AKC slow / git ssh hangs at "Authenticating"

**Symptom:** `git clone git@…` sits at the auth phase for many
seconds before completing or timing out.

The `AuthorizedKeysCommand` invokes `/usr/local/bin/shithubd
ssh-authkeys %f`, which queries Postgres. Latency comes from one
of:

- DB connection saturation. `pgxpool` is shared across the web
  process; the AKC subcommand opens its own connection. Check
  `pg_stat_activity` for stuck connections.
- DNS for the AKC subprocess (it should not need DNS, but a
  misconfigured `/etc/nsswitch.conf` can stall stat calls).
- The user has a *very* large number of keys; AKC walks all of
  them. Practical maximum is in the hundreds before you notice.

Mitigation: investigate, don't bypass. Disabling the AKC opens a
hole — every key on disk would have shell access.

## Job queue: backlog growing

**Symptom:** `shithubd_job_queue_depth > 5000` for 15+ minutes.

```sh
psql -d shithub -c "
  SELECT kind, count(*) FROM jobs WHERE state='queued'
  GROUP BY kind ORDER BY 2 DESC;"
```

If one kind dominates, look for a poison job in worker logs:

```sh
journalctl -u shithubd-worker -n 500 --grep ERROR
```

If the spread is even, the worker isn't keeping up — scale by
running a second worker host. The `FOR UPDATE SKIP LOCKED`
pattern lets multiple workers coexist safely.

To purge a poison job: mark it `failed` in SQL (don't delete —
you want the audit trail).

## Webhook: deliveries failing 50%+

**Symptom:** `WebhookDeliveryFailing` alert.

- A specific subscriber URL is broken. Check the webhook's
  delivery log in the UI (`/owner/repo/settings/hooks/{id}`) for
  status codes and bodies.
- Outbound network issue. From the host:
  `curl -v https://example.com/hook`. If outbound HTTPS itself
  is broken, that's an infrastructure problem.
- Auto-disable kicks in after 50 consecutive failures per
  webhook; the UI shows a banner and sets `active=false`. Owner
  re-enables once the endpoint is fixed.

## Push: "pre-receive hook declined"

**Symptom:** User's `git push` fails with a pre-receive
rejection.

Common reasons:

- **Branch protection.** Pushing directly to a protected branch
  when the rule requires a PR. User pushes to a feature branch
  instead.
- **Repo over quota.** Bare-repo size cap hit; raise per-repo or
  globally in config.
- **Pack contains too-large object.** Large blobs blow the
  per-object cap (defaults documented in `internal/repos`). Use
  LFS instead for big binaries.

## Postgres: archive_command failing

**Symptom:** `pg_stat_archiver.failed_count` increasing; disk
filling on the db host.

`journalctl -u postgresql -n 200 --grep "archive_command failed"`
will print the rclone error. Common causes:

- Rotated bucket credentials.
- Network partition to Spaces.
- `rclone.conf` missing or unreadable.

Confirm by hand:

```sh
sudo -u postgres rclone --config /root/.config/rclone/rclone.conf \
     lsd spaces-prod:
```

Fix the underlying issue; Postgres retries automatically. If disk
is critically full and Spaces won't be reachable in time, page
the operator. **Do not** `pg_archivecleanup` manually unless
explicitly directed — you can lose data.

## Email: signups never arrive

- `auth.email_backend=stdout` in your config — emails went to the
  journal. Check `journalctl -u shithubd-web --grep '\[email\]'`.
- Postmark token wrong / sender domain not verified. Postmark's
  dashboard logs every send.
- DKIM/SPF not set up; the provider may accept and silently drop.

For test, send a verification email to an address you control on
a domain that bounces with helpful errors (e.g., your own MTA
configured to log rejections).

## Sign-in fails immediately with no error in journal

CSRF token expired (cookie didn't match the form's hidden token).
Force a hard reload of the sign-in page; cookies older than the
session lifetime are dropped.

If that's not it: check `auth.require_email_verification=true` —
the user may need to click a verification link.

## "Site appears slow" with no specific symptom

Top sources of latency, in order of frequency:

1. **N+1 query regression.** DB calls/sec on the Grafana
   dashboard will be much higher than baseline.
2. **GC pressure.** The web process is allocating more than usual
   — check Grafana's `go_gc_pause_seconds` panel.
3. **Cache stampede.** If a hot cache key was just invalidated,
   first-request-wins via singleflight should prevent this; if
   it doesn't, check `cache.singleflight_in_flight` for excess.
4. **Disk-bound on git ops.** Concurrent large clones pin the
   web host's disk. Move git transports to a separate process
   or scale the web tier.
