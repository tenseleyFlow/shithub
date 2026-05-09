# Threat model — v1

A short, concrete document describing who shithub defends against,
what we protect, and how the controls map to attackers. This is the
v1 baseline; it evolves with the codebase. Per the S35 spec, "two
or three pages, not thirty." Pair with `security-checklist.md` for
the per-control test references.

## Assets

In rough decreasing order of consequence if compromised:

1. **User credentials.** Password hashes, TOTP secrets, recovery
   codes, PATs, SSH public keys. Direct path to account takeover.
2. **Repository content.** Source code, issues, PR discussions,
   private-repo files. Privacy + IP value; for some users, source
   IS the product.
3. **Webhook secrets + delivery payloads.** A leaked webhook secret
   lets an attacker spoof events to subscriber systems. Payloads
   often contain user content that hadn't been published.
4. **Site-admin actions.** Suspending users, deleting repos,
   impersonating accounts. Trust violations here cascade.
5. **Server-side resources.** CPU (argon2 hashing, git ops), disk
   (repo storage, response-body cap), DB connections.

## Attackers

### A1 — Compromised user account

The attacker authenticates as a real user (phished password, leaked
PAT). They have whatever the user has.

**Mitigations:**
- Per-account session-epoch invalidation: the user can "log out
  everywhere" and burn every session at once.
- TOTP 2FA gate (S06) — when enabled, the password alone doesn't
  log in. Recovery codes one-shot.
- Audit log on security-relevant actions (auth, settings, admin)
  surfaces the attack to the user + admins.
- Suspended-account gate (`policy.Can`) lets an admin freeze a
  hijacked account without nuking the user's data.
- Common-password blocklist + argon2id make password-spraying
  impractical.

### A2 — Malicious public-repo viewer

An anonymous or freshly-signed-up user crafts payloads against the
public-repo surface: XSS via issue body, CSRF from a public-org page,
SSRF via a webhook to internal infrastructure.

**Mitigations:**
- All user content is markdown-rendered through `internal/markdown`
  with `bluemonday` UGC policy; ad-hoc `template.HTML(...)` is
  lint-blocked outside designated render helpers.
- CSRF protection at the router root (nosurf-derived); state-
  changing routes inherit it. PAT-only API and git transports are
  explicit exemptions documented in the lint script.
- SSRF defense (`internal/security/ssrf`) on every outbound HTTP:
  IP block-list + dial-the-IP transport defeats DNS rebinding.
- Webhook secret-decrypt failure auto-disables the hook so a
  compromised key doesn't keep delivering forever.
- CSP, Frame-Options DENY, COOP, CORP cut off the embed/clickjack
  surface.

### A3 — Abusive signup automation

Botnets create accounts in bulk to spam issues, host abuse content,
or burn through resources.

**Mitigations:**
- Per-IP signup throttle (S05): 5/hour.
- Per-/24 signup throttle (S35): 20/hour. Catches spray-from-many-
  IPs-on-the-same-network patterns.
- Honeypot field on the signup form (silently treats success).
- Email verification required (configurable) gates account
  activation behind a real inbox.
- Captcha integration is deferred (vendor decision pending); the
  per-/24 throttle is the live primary defense.

### A4 — Supply-chain via webhook subscriber

A compromised subscriber URL receives webhook deliveries and uses
the captured payloads to escalate (phishing, replay).

**Mitigations:**
- HMAC-SHA256 signing on every delivery; subscribers can verify
  authenticity. Per-webhook secret stored AEAD-encrypted at rest.
- Idempotency key on each delivery so replays are detectable.
- SSRF defense rejects subscriber URLs that resolve to private IPs
  (operator can opt in for self-hosted CI via `AllowedHosts`).
- Auto-disable on persistent failure (50 consecutive). Damage is
  bounded.

### A5 — Insider with admin access

A site admin abuses privileges (looking at user data, mass-deleting
repos, impersonating users to act on their behalf).

**Mitigations:**
- Impersonation defaults read-only (`policy.Can` +
  `DenyImpersonationReadOnly`); writes require a typed-name confirm.
- Visible red sticky banner on every page during impersonation.
- Audit row carries BOTH the real admin id and the impersonated id
  (`meta.impersonated_user_id`) for forensics.
- Bootstrap-admin CLI is the only out-of-band elevation path; all
  subsequent grants happen through `/admin/users/{id}` and audit.
- 404 (not 403) for non-admin `/admin` access prevents privilege
  enumeration.

### A6 — Resource exhaustion

A determined attacker tries to consume CPU, DB connections, or disk
faster than our limits.

**Mitigations:**
- Body-size caps on auth POSTs (so 10MB password bodies don't burn
  argon2 cost).
- Repo-create throttle (10/hour/user); content-creation throttles
  on issues/comments/stars/forks.
- Webhook payload cap (25 MiB); response body cap (32 KiB stored).
- Job queue uses `FOR UPDATE SKIP LOCKED` so contention bounded.
- pgx pool max-conns capped per process.

## Out of scope (v1)

Documented here so they don't get assumed:

- **State-actor adversaries.** No claim to defend against an attacker
  with control over CDN/DNS/CA infrastructure.
- **Side-channel attacks on the host.** Spectre/Meltdown class
  defenses are the OS/runtime's responsibility.
- **Physical access to servers.** Standard ops practice; not in app
  layer.
- **Coordinated disclosure pipeline.** Future work.
- **Penetration test by external party.** Future work.

## Review cadence

This document is reviewed at the start of every security-touching
sprint (S35, S39 beta hardening) and on any major architecture
change (S37 deploy, S44 GraphQL API). Significant updates require a
PR with an explicit reviewer note in the description.

## S39 hardening review (2026-05-09)

The S39 internal pen-test (3 days, scoped to the OWASP top set +
auth + git + webhook SSRF) noted the following considerations
for v1 — none introduce a new attacker class, but they sharpen
how A1–A6 are addressed:

- **A1 — compromised account.** The S38 introduction of the
  finalized "sign out everywhere" surface (per-account session
  epoch) is the operator's primary lever. The audit flagged that
  rotating the session signing key
  (`docs/internal/runbooks/rotate-secrets.md`) is also a global
  kill-switch — useful for "we suspect the cookie database
  leaked." Documented; no code change.
- **A2 — public viewer.** The render.go fix landing in S39
  (`internal/web/render/render.go`) closes a class of silent-
  blank-page bugs that, while not a vulnerability themselves,
  made it harder to notice missing authorization gates during
  development. Fail-loud at parse time is now the rule.
- **A4 — webhook subscriber.** The SSRF defense
  (`internal/security/ssrf/`) gets re-tested every release; S39
  added the `audit-a11y` and `load-test` CI scaffolding but did
  not change the SSRF surface.
- **A6 — resource exhaustion.** The k6 scenarios in
  `tests/load/k6/scenarios/` exercise the rate-limit floors. The
  S39 spec calls out "0% 5xx errors; rate-limit-driven 429s
  expected and counted" — confirmed in the load-test design.

## Out-of-band watchlist (track separately)

These don't fit the A1–A6 attacker model but operators should
keep an eye on them:

- **Dependency-supply-chain on the Go side.** `go.sum` pinning
  is enforced; we don't yet do reproducible-build verification.
- **The docs subdomain serving from Spaces.** A bucket
  policy mistake there could let an attacker stage a phishing
  page on `docs.shithub.example`. Mitigated by Caddy's CSP
  and the explicit reverse-proxy origin
  (`deploy/docs-site/Caddyfile.snippet`).
- **PAT prefix recognition by external secret scanners.**
  `shp_` is documented in `docs/public/user/personal-access-
  tokens.md` and recognised by GitGuardian/GitHub's scanners;
  if we ever rotate the prefix, coordinate with them so leaked
  tokens still get caught upstream.
