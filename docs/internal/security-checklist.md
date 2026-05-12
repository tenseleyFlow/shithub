# Security checklist

The S35 baseline. Each row names the control, the artifact that
enforces it (test/lint), and the sprint that landed it. Update on
every security-relevant change; the checklist is the canonical
"what does shithub claim to defend against, and where do we prove it"
document.

## Identity & authentication

| Control | Enforced by | Sprint |
|---|---|---|
| Argon2id password hashing | `internal/auth/password` + tests | S05 |
| Common-password blocklist | `internal/passwords` + signup gate | S05 |
| Per-IP login throttle | `internal/auth/throttle` + tests | S05 |
| Per-IP signup throttle | `auth.throttleSignup` (S05 layer) | S05 |
| Per-/24 signup throttle | `internal/ratelimit.AllowSignupIP` + tests | S35 |
| TOTP 2FA + recovery codes | `internal/auth/totp` + tests | S06 |
| TOTP secret AEAD-encrypted at rest | `internal/auth/secretbox` (chacha20poly1305) | S06 |
| Session cookie `Secure; HttpOnly; SameSite=Lax` | `internal/auth/session/CookieStore` | S02 |
| CSRF cookie `Secure; SameSite=Strict` | nosurf middleware config | S02 |
| Session epoch invalidation ("log out everywhere") | `users.session_epoch` + middleware bump check | S05 |
| Constant-time login (no timing oracle) | `password.Compare` HMAC-cmp | S05 |
| Reset / verification token entropy = 32 bytes | `internal/auth/token` + tests | S05 |
| Tokens hashed at rest | `token.HashOf` + sqlc storage | S05 |

## Authorization

| Control | Enforced by | Sprint |
|---|---|---|
| Single policy entrypoint (`policy.Can`) | `scripts/lint-policy-boundary.sh` in `make ci` | S15 + audit |
| 404 (not 403) for non-admin `/admin` access | `middleware.RequireSiteAdmin` + tests | S34 |
| Impersonation defaults read-only | `policy.Can` + `DenyImpersonationReadOnly` + tests | S34 |
| Impersonation audit captures real + impersonated id | `admin.recordAdminAction` | S34 |
| Org suspension blocks writes | `policy.Can` + `DenyOrgSuspended` | S30 |
| Repo soft-delete blocks all actions | `policy.Can` + `DenyRepoDeleted` | S15 |
| Author-self-close on issues/PRs | `policy.Can` author branch | S21/S22 |
| Paid org feature gates stay behind entitlements | `scripts/lint-org-plan-boundary.sh` in `make ci` | PAYMENTS SP01 |

## Input handling

| Control | Enforced by | Sprint |
|---|---|---|
| Markdown sanitizer is the only `template.HTML(...)` user-content path | `scripts/lint-markdown-boundary.sh` in `make ci` | S25 + audit |
| `goldmark`/`bluemonday` only in `internal/markdown` | same as above | S25 |
| Body-size cap on auth POSTs | `middleware.MaxBodySize` | S05 |
| URL parameter validation (e.g. `next=`) | `internal/security/openredirect` + tests | S35 |
| File-extension validation on uploads | `internal/avatars` (avatars only path today) | S10 |

## Outbound HTTP / SSRF

| Control | Enforced by | Sprint |
|---|---|---|
| DNS resolve + IP block-list (RFC1918, loopback, ULA, CGNAT, …) | `internal/security/ssrf.IsForbiddenIP` + tests | S33 → S35 |
| Dial-the-IP transport (defeats DNS rebinding) | `ssrf.Config.HTTPClient` | S33 → S35 |
| No follow on 3xx (SSRF amplifier) | `ssrf.Config.HTTPClient.CheckRedirect` | S33 |
| Scheme/port allow-list | `ssrf.Config.Validate` + tests | S33 → S35 |
| Per-webhook signing (HMAC-SHA256) | `internal/webhook.SignSHA256` + tests | S33 |
| Webhook secrets AEAD-encrypted at rest | `internal/webhook.SealSecret` | S33 |

## Rate limiting & anti-abuse

| Control | Enforced by | Sprint |
|---|---|---|
| Generalised counter table | `internal/ratelimit` + `rate_limits` migration | S35 |
| `X-RateLimit-Limit/Remaining/Reset` headers + `Retry-After` | `ratelimit.Middleware` + `StampHeaders` | S35 |
| Per-IP rate limit (anonymous) | `ratelimit.IPKey` + middleware | S35 |
| Per-/24 signup throttle | `ratelimit.AllowSignupIP` (above) | S35 |
| Repo-create throttle (10/hour/user) | `repos.Create` throttle hit | S11 |
| Star/unstar throttle (100/hour/user) | `social` package throttle | S26 |
| Email-bomb defense (per-email reset/verify caps) | `internal/auth/email` per-address gates | S05 |

## Headers, cookies, CSP

| Control | Enforced by | Sprint |
|---|---|---|
| Content-Security-Policy (default-src 'self', `frame-ancestors 'none'`, …) | `middleware.SecureHeaders` + tests | S02 |
| HSTS (when TLS or trusted proxy) | `SecureHeaders` + `requestIsTLS` | S02 |
| `X-Frame-Options: DENY` | `SecureHeaders` | S02 |
| `Referrer-Policy: strict-origin-when-cross-origin` | `SecureHeaders` | S02 |
| `Permissions-Policy` (deny risky surfaces) | `SecureHeaders` | S02 |
| `Cross-Origin-Opener-Policy: same-origin` | `SecureHeaders` | S02 |
| `Cross-Origin-Resource-Policy: same-origin` | `SecureHeaders` | S02 |
| `X-Content-Type-Options: nosniff` | `SecureHeaders` | S02 |
| Content-Type set BEFORE WriteHeader on 4xx/5xx paths | `auth.writeRetryAfter`, `render.Render` | S05 fix |

## Secrets hygiene

| Control | Enforced by | Sprint |
|---|---|---|
| No token-prefix patterns in non-exempt source | `scripts/lint-secret-logs.sh` in `make ci` | S35 |
| URL-redactor strips `user:pat@host` from logged URLs | `internal/auth/pat` redactor | S08 |
| AEAD key rotation procedure documented | `docs/internal/2fa.md` | S06 |
| Webhook secret-decryption failure auto-disables hook | `webhook.Deliver` + `AutoDisableWebhook` | S33 |

## Actions / CI runner

| Control | Enforced by | Sprint |
|---|---|---|
| Workflow parser rejects unsupported `uses:` aliases and unknown keys | `internal/actions/workflow` + parser tests | S41a |
| Event-derived expressions are tainted and never spliced directly into shell text | `internal/actions/expr` + runner env-binding tests | S41a/S41d |
| Actions secrets encrypted at rest | `internal/actions/secrets` + secretbox round-trip tests | S41c |
| Claim-time secret mask snapshots survive later secret rotation/deletion | `workflow_job_secret_masks` + runner API log tests | S41e |
| Runner job API JWTs are short-lived and single-use | `runner_jwt_used`, `runnerjwt`, runner API replay tests | S41c |
| Checkout JWTs are separate, repo-scoped, and read-only | git HTTP checkout-token tests | S41h |
| Runner step containers drop privileges and capabilities | Docker engine argv tests + runner deploy runbook | S41d/S41e |
| Runner bridge blocks direct-IP egress and workflow-supplied DNS bypasses | `deploy/runner-config/{dnsmasq,firewall}.j2` + deploy runbook smoke | S41e |
| Logs are scrubbed runner-side and server-side | `internal/runner/scrub`, server log path tests, scrub metrics | S41e |
| Actions observability alerts cover stale runners, queue depth, p99 regression, and scrubber health | Prometheus rules + `make audit-actions-ga` | S41h |
| Full project CI dogfood remains canary-only until marketplace/toolchain gaps close | `docs/internal/actions-ga-readiness.md` + `make audit-actions-ga` | S41h |

## Operator controls

| Control | Enforced by | Sprint |
|---|---|---|
| `bootstrap-admin` CLI | `cmd/shithubd/admin.go` | S34 |
| Force-archive / force-delete (admin) | `internal/web/handlers/admin/repos.go` | S34 |
| Job retry/discard (admin) | `internal/web/handlers/admin/jobs.go` | S34 |
| Audit-log viewer | `internal/web/handlers/admin/audit.go` | S34 |
| Site-admin CLI bootstrap audit row | `audit.ActionAdminSiteAdminGranted` | S34 |

## Future tightening (deferred)

These are tracked here so they don't atrophy. Each is small enough
to land in a follow-up sprint but didn't make S35's cut:

- **Tighten CSP `script-src` to drop `'unsafe-inline'`.** The S02
  default still accepts inline scripts because of the theme-flash
  avoider in `_layout.html`. Replace with `'sha256-…'` of the
  exact inline block (or move the script to an external file).
- **Captcha integration on signup.** Per-/24 throttle is the
  current defense; captcha vendor decision (hCaptcha vs Cloudflare
  Turnstile) is deferred to a follow-up. The throttle is the gate
  the captcha plugs into.
- **Authorization-scope lint for API routes.** Per-route scope
  decoration + a `scripts/lint-scopes.sh` that asserts every
  registered API handler declares its required scope. Deferred
  until the API surface grows past the current minimal shape.
- **Open-redirect allowlist plumbing into handlers.** The validator
  exists (`internal/security/openredirect`); per-handler use sites
  (`?next=` consumers in auth, settings) will adopt it as the
  surfaces evolve.
