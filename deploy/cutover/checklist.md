# Cutover checklist

The S40 launch checklist. Walk it top-to-bottom on cutover day;
do not skip steps. Each box has a verification command or a
visual check.

> **Time-box.** A clean run is ~45 min from "ssh in" to "signup
> open." Budget 90 min; stop and back out if you hit ~2 hours.

## T-7 days

- [ ] DNS A/AAAA for `shithub.example` published with low TTL
      (300s) so cutover-day changes propagate fast. Verify:
      `dig +short A shithub.example`.
- [ ] DNS CNAME for `docs.shithub.example` published.
- [ ] Postmark domain verified; SPF/DKIM/DMARC aligned. Verify:
      Postmark dashboard → Domains → green.
- [ ] Signup-throttle config reviewed; per-IP and per-/24
      ceilings tuned for the announcement bump.
- [ ] Monitoring alerts wired to the on-call's Telegram + SMS.
      Test by triggering a synthetic `BackupOverdue` alert via
      Alertmanager API and confirming it pages.
- [ ] Rollback rehearsed on staging:
      `git checkout v0.999 && make deploy ANSIBLE_INVENTORY=staging`.

## T-48 hours

- [ ] Last DNS change committed. Cutover after 48h ensures no
      propagation lag.
- [ ] S37 backup-restore drill green within last 7 days.
- [ ] S38 docs deploy verified; `https://docs.shithub.example/`
      returns 200.
- [ ] S39 P0/P1 bugs closed.
- [ ] Tag the release commit:
      ```sh
      git tag -a v1.0.0 -m "v1.0.0 — launch"
      git push origin v1.0.0
      ```

## T-1 hour

- [ ] On-call has phone + laptop reachable.
- [ ] Status page updated to "Cutover in progress" (manual edit
      to `docs/public/status.md`, push, sync to docs bucket).
- [ ] `caddy_use_acme_staging=false` in production inventory
      (so the cutover doesn't accidentally fall back to LE
      staging).

## T-0: cutover

```sh
# 1. Pull the v1.0.0 tag.
git fetch --tags
git checkout v1.0.0

# 2. Dry-run to confirm exactly what will change.
make deploy-check ANSIBLE_INVENTORY=production

# 3. Apply. Expect ~10s downtime as the web service restarts.
make deploy ANSIBLE_INVENTORY=production
```

The Ansible run includes `shithubd migrate up` as the web
service's `ExecStartPre`. New migrations run as part of the
restart; the unit stays in `activating` until they complete.

Watch:

```sh
ssh web-01
journalctl -fu shithubd-web
```

## Smoke

Run the smoke script as soon as the deploy reports `ok=N
changed=N failed=0`:

```sh
deploy/cutover/smoke.sh https://shithub.example
```

The script exercises: home page, signup form, login form, health
endpoints, docs subdomain, a representative API call. Exits
non-zero on any 5xx or unexpected response shape.

## Bootstrap-admin

```sh
ssh web-01
sudo -u shithub /usr/local/bin/shithubd admin bootstrap-admin \
     --email you@yourdomain
```

The CLI prints a one-time password-reset link. Open in a browser,
set a password, **immediately enable 2FA** (Settings → Account
security).

## Open signup

If signup was gated behind a feature flag during the pre-launch
build:

```sh
ssh web-01
sudo systemctl edit shithubd-web --full
# remove SHITHUB_AUTH__SIGNUP_DISABLED=true (or set to false)
sudo systemctl restart shithubd-web
```

Otherwise signup is already on; verify via the signup form
returning 200 + a valid CSRF token.

## Mirror to GitHub

Set up the one-way mirror so the GitHub mirror keeps receiving
pushes:

```sh
# On the web host, as the shithub user:
cd /data/repos/shithub/shithub.git
git remote add github https://github.com/tenseleyFlow/shithub.git
# Add the mirror push to the periodic worker job (covered by
# the worker config; the mirror job kind = "git.mirror_push").
```

Confirm a test push lands on both:

```sh
git clone https://shithub.example/shithub/shithub.git /tmp/test-clone
cd /tmp/test-clone
echo "launch test" >> .launch-test
git add .launch-test
git commit -m "launch smoke push"
git push origin trunk
# Wait ~60s for the mirror job to run, then confirm on GitHub:
git ls-remote https://github.com/tenseleyFlow/shithub.git trunk
```

## Status page

Update `docs/public/status.md` to "All systems normal." with the
current timestamp; push, sync to docs bucket.

## Announcement

Schedule the announcement post for **Tuesday 09:00 ET** (or your
chosen window). Submit to:

- [ ] Hacker News: title + URL only; first comment is the
      "What is shithub?" intro.
- [ ] /r/programming, /r/selfhosted: link + summary, follow
      subreddit rules.
- [ ] lobste.rs: title + URL.
- [ ] Mastodon: short post + link.

Have the FAQ tab open; expect "is this Forgejo?" / "why not
Codeberg?" / "where's CI?" within the first hour.

## Day-zero monitoring

For the first 24h:

- Refresh Grafana every 30 min.
- Triage every alert immediately; nothing false-positive should
  page in week 1 (we tuned for it).
- Bug reports go to `https://shithub.example/shithub/shithub/issues`
  (the project's own self-hosted issues — drink your own
  champagne).

## Backout

If cutover goes sideways within the first hour:

1. **Stop the bleed.** Put the site in read-only mode
   (`docs/internal/runbooks/read-only-mode.md`).
2. **Decide:** roll back code, restore data, or wait?
3. If rolling back code: `deploy/cutover/rollback.sh v0.999`.
4. Status page → "Investigating" with what we know.
5. Page the operator (yourself, by definition).

The 24h SLO is "report what we know, not promises about when it's
fixed." Honesty wins trust; deadlines under stress lose it.

## Day-one retro

After the first 24h, fill in `docs/internal/retro/v1.0.0.md`
with: what worked, what surprised us, top 3 user-reported
issues, and the next sprint's focus.
