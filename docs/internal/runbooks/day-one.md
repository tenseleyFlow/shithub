# Day-one operator runbook

This is what the on-call (the project author for v1) reads on
launch day and the day after. Pair with the cutover checklist
(`deploy/cutover/checklist.md`) for the actual cutover steps.

## "It's been an hour"

Open three tabs and walk this list:

1. **Grafana → shithubd-overview dashboard.**
   - `Web up` and `Worker up` should both be 1.
   - p95 latency on top routes should sit near baseline (S36
     numbers, doubled is the soft ceiling — see
     `docs/internal/capacity.md`).
   - DB calls/sec should NOT be 10× baseline. If it is, an N+1
     regression slipped through; check `journalctl -u shithubd-web
     --grep 'high-query-count'` (the QueryCounter middleware
     emits warning lines).

2. **Loki → `{service="shithubd"} |~ "panic|ERROR"`.**
   - Zero panics is the bar.
   - One or two ERROR lines from genuinely broken third-party
     subscribers is normal (we auto-disable after 50; this is
     fine).
   - A flood of ERROR lines from the same handler is a bug —
     bisect.

3. **Status page on docs.shithub.example/status.html.**
   - Confirm "All systems normal." with a current timestamp.
   - If you've had any blip, even a 30-second one, log it under
     "Recent incidents" with what happened. Trust comes from
     transparency, not from the page being empty.

If all three look clean, take a breath. Then post the announcement
if you haven't.

## "It's been a day"

24h post-cutover checklist:

1. **Backups.** The first daily logical backup should have run.
   ```sh
   ssh db
   sudo -u postgres rclone --config /root/.config/rclone/rclone.conf \
        lsf spaces-prod:shithub-backups/daily/$(date -u +%Y/%m/%d)/
   ```
   Should list one `.dump` file. If empty, see
   `docs/internal/runbooks/backups.md#missed-backup`.

2. **WAL archive.** `pg_stat_archiver.failed_count` should be 0
   and `last_archived_time` should be within minutes of now.
   ```sh
   ssh db
   sudo -u postgres psql -d shithub -c "SELECT * FROM pg_stat_archiver;"
   ```

3. **Mirror push to GitHub.** The mirror job runs hourly. Confirm:
   ```sh
   git ls-remote https://github.com/tenseleyFlow/shithub.git trunk
   git ls-remote https://shithub.example/shithub/shithub.git trunk
   ```
   Both should report the same SHA (within an hour of the latest
   push to shithub).

4. **Signups + abuse.** Check `/admin/users` for the past 24h.
   - Anything obviously bot-shaped (timing, email pattern,
     username pattern)? The per-/24 throttle should have caught
     bursts; a long tail of single-account spam needs manual
     review.
   - Suspended accounts: log who and why for the retro.

5. **Issue tracker.** Open
   `https://shithub.example/shithub/shithub/issues`. Triage every
   new issue:
   - **P0** — site down / data loss / security. Fix immediately.
   - **P1** — broken core flow (signup, push, merge). Fix this
     week.
   - **P2** — minor bug / UX. Fix in next sprint.
   - **P3** — feature ask / WONTFIX / accept-with-rationale.
   - Reply on every issue within 24h, even if just "tracked, P2."

6. **Retro.** Update
   `docs/internal/retro/v1.0.0.md` — fill the "Numbers" table,
   the "What surprised us" section, the top-3 issues table.

## "First incident?"

The general procedure is in
`docs/internal/runbooks/incidents.md`. Day-one specifics:

1. **Status page first.** Within 5 min of detecting an incident,
   update `docs/public/status.md` to "Investigating: <one line>".
   Push, sync to docs bucket. The fact that you're aware is
   half the battle.
2. **Identify the right runbook.** Most v1 incidents will be one
   of:
   - `incidents.md#shithubd-down` — process crashed.
   - `incidents.md#postgres-down` — DB unreachable.
   - `incidents.md#archive-failing` — WAL archiver broken.
   - `incidents.md#job-backlog` — worker queue runaway.
   - `incidents.md#webhook-delivery-failing` — outbound delivery
     issues.
3. **Mitigate before fix.** If it's the announcement spike
   tipping the rate-limit floor, you can briefly raise the floor
   (config edit + restart) rather than untangling the abuse
   signal. The runbook + the rate-limit-tuning notes are both at
   hand.
4. **Status page on resolution.** "Resolved at HH:MM UTC. <One
   sentence on cause.> <One sentence on prevention if relevant.>"

## Communication posture

- **Honest > fast.** If you don't know the cause, "investigating"
  is a fine status. Speculation in public is a debt you pay
  later.
- **No timelines under stress.** "We'll have an update by 18:00
  UTC" is fine. "We'll have a fix by 18:00 UTC" is a trap.
- **Engage the reporter.** When a user files a serious bug,
  thank them on the issue. Even if the fix is days out, the
  acknowledgement is the part they remember.

## Rate-limit tuning

If legitimate signups are getting throttled (CIDR-shared
networks during the announcement spike), the steps are:

```sh
# 1. Edit the production inventory's rate-limit vars.
$EDITOR deploy/ansible/inventory/production
#   signup_per_ip_per_hour: 5  → 10
#   signup_per_24_per_hour: 20 → 50

# 2. Apply only the app role (no DB / edge churn).
make deploy ANSIBLE_INVENTORY=production ANSIBLE_TAGS=app
```

The web service restart picks up new env. Document the change in
the retro; tighten back to defaults once the spike passes.

## "Drop the GitHub mirror?" decision

Default plan: **keep the mirror for 90 days post-launch**, then
re-evaluate. The mirror exists as a recovery surface; if the
self-hosted instance has had no extended outage in 90 days,
drop the mirror push job and mark the GitHub repo as archived.

The decision and date land in the next retro after the 90-day
mark.

## When to escalate (to whom?)

Solo on-call for v1. There is no second-tier. Escalation paths:

- **DB destroyed beyond restore.** Restore from cross-region DR
  bucket per `runbooks/restore.md`.
- **Account compromise on the operator's email.** Revoke the
  compromised email's session epoch via SQL; trigger a
  "rotate-secrets" pass.
- **Legal request.** Acknowledge receipt; do not respond to the
  substance same-day.
- **Mass-abuse incident.** Read-only mode, then triage. The
  read-only-mode runbook is the lever.
