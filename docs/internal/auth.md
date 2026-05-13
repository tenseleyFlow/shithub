# Authentication

S05 brings the first real domain surface to shithub: email/password signup, email verification, login, logout, password reset, and rate-limiting. This doc covers what's wired, where the code lives, and the security choices behind the design.

## What this sprint ships

- Real users in Postgres: `users`, `user_emails`, `password_resets`, `email_verifications`, `auth_throttle`, `username_redirects`.
- argon2id password hashing with PHC string encoding.
- Strict username whitelist + reserved-name list.
- Common-password rejection from an embedded SecLists 10k corpus.
- Email Sender abstraction with stdout (dev), SMTP (MailHog/local), Postmark, and Resend backends.
- Counter-based rate-limiting backed by Postgres (no Redis dep).
- Auth handlers: signup, login, logout, password reset (request + confirm), email verification, verification resend.
- Constant-time login: missing usernames still trigger an argon2 hash against a pre-computed dummy.
- Generic password-reset response: no user-existence enumeration via that flow.
- 4 KiB request-body cap on auth POSTs (anti-DoS for the hashing path).
- Honeypot field on the signup form.
- `shithubd admin reset-password <username>` operator escape hatch.

## Out of scope (future sprints)

- 2FA / TOTP (S06)
- SSH keys (S07)
- Personal access tokens (S08)
- OAuth providers (post-MVP)
- Profile / settings UI (S09 / S10)

## Tables (migrations 0002–0008)

| Table | Purpose | Notes |
|---|---|---|
| `users` | identity + password material | `username citext UNIQUE`. PHC argon2id hash. Soft-deletable (`deleted_at`) and suspendable. |
| `user_emails` | one or more emails per user | `email citext UNIQUE` across all users. Partial unique index enforces one primary per user. |
| `password_resets` | reset tokens | `token_hash bytea UNIQUE`. 1-hour TTL. Single-use via `used_at`. |
| `email_verifications` | verification tokens | `token_hash bytea UNIQUE`. 24-hour TTL. Single-use via `used_at`. |
| `auth_throttle` | rate-limit counters | `(scope, identifier)` UNIQUE. Fixed-window. |
| `username_redirects` | username-change history | Old name → new user. Acts as a 30-day reservation (S10 wires the cooldown). |

`citext` is enabled in migration 0002 for case-insensitive uniqueness without functional indexes. Display casing for users and emails lives in the same column — citext preserves it on read.

## Password hashing

`internal/auth/password` implements argon2id using `golang.org/x/crypto/argon2`. Defaults:

- Memory: 64 MiB (`m=65536`)
- Time: 3 iterations (`t=3`)
- Threads: 2 (`p=2`)
- Salt: 16 bytes
- Key: 32 bytes

These are tuned for ~100–300 ms per Hash on dev hardware. Operators can override via `auth.argon2.*` config. The output is a canonical PHC string:

```
$argon2id$v=19$m=65536,t=3,p=2$<saltB64>$<hashB64>
```

Stored in `users.password_hash`; `users.password_algo` is `argon2id-v1` so we can roll forward later.

A package-level `dummyEncoded` is computed on first use. Login handlers call `password.VerifyAgainstDummy` when the username doesn't exist so the response time matches a real failed-password verification (defense against username enumeration via timing).

The minimum password length is 10 characters. There is **no** maximum — argon2id can hash arbitrary input — but the auth-POST middleware caps `r.Body` at 4 KiB so a misbehaving client can't ship a 10 MB password to weaponize the hashing path.

## Tokens

`internal/auth/token` mints 32-byte cryptographically-random tokens, base64url-encodes them for inclusion in URLs/emails, and stores their sha256 hash in the DB. The raw token never touches disk. Lookups parse the URL fragment, hash, and query by hash with a UNIQUE-index O(1) seek. Comparisons use `crypto/subtle.ConstantTimeCompare`.

## Reserved-name list

`internal/auth/reserved.go` maintains a set of usernames that may not be claimed: every static top-level route shithub registers, GitHub-known reservations we mirror for parity, and a buffer of likely-future routes. Signup checks this list (case-insensitive — `LOGIN` is rejected the same as `login`).

When a future sprint adds a new top-level route, the leading path segment **must** be added here. The intended audit mechanism: `internal/web/handlers/handlers_test.go` should walk the registered chi routes and check every static segment against `auth.ReservedNames()`. (Stub-level today; full route-walking lands when the route surface stabilizes in S11+.)

## Common-password list

`internal/passwords` embeds the SecLists `10k-most-common.txt` corpus (~73 KB on disk). `IsCommon` is case-insensitive. Used at signup and password-reset.

To refresh: replace `internal/passwords/common_passwords.txt` with a newer SecLists snapshot and re-run the test suite.

## Email service

`internal/auth/email` defines the `Sender` interface. Four implementations:

- `StdoutSender` — writes a human-readable dump to a writer. Default in dev when no SMTP is configured. Convenient for tests.
- `SMTPSender` — plain SMTP for MailHog locally. Authenticated and TLS-upgrade variants supported.
- `PostmarkSender` — Postmark transactional API.
- `ResendSender` — Resend transactional API (https://resend.com). Comparable feature set to Postmark; preferred where onboarding speed matters (no human approval queue).

`messages.go` hosts the `VerifyMessage` and `ResetMessage` builders. Both produce HTML + plaintext bodies — every transactional email shithub sends works in plain-text-only clients. Templates are inlined (short, rarely change); when they grow, promote to `templates/email/*.{html,txt}`.

### Wiring

`auth.email_backend` chooses the implementation: `stdout | smtp | postmark | resend`. The `smtp` backend additionally requires `auth.smtp.addr`; `postmark` requires `auth.postmark.server_token`; `resend` requires `auth.resend.api_key`. Validation enforces these.

```sh
# Dev: capture mail in MailHog
make dev-email
SHITHUB_AUTH__EMAIL_BACKEND=smtp ./bin/shithubd web
# Open http://127.0.0.1:8025 to read captured messages
```

## Rate limiting

`internal/auth/throttle` implements fixed-window counters via the `auth_throttle` table. The `BumpAuthThrottle` query atomically increments the counter for `(scope, identifier)` or starts a new window if the existing one is older than `(now - Window)`.

Limits enforced by the auth handlers:

| Scope | Identifier | Max | Window | Where |
|---|---|---|---|---|
| `signup` | `ip:<client-ip>` | 5 | 1h | `signupSubmit` |
| `login` | `ip:<client-ip>\|<username>` | 6 | 15m | `loginSubmit` (reset on success) |
| `reset` | `email:<addr>` | 3 | 1h | `resetRequestSubmit` |

429 responses include a `Retry-After` header.

We deliberately use Postgres rather than introducing Redis. At launch scale this is well within Postgres's comfort zone, and avoiding a new dep is worth the marginal latency. Migrate if S36 proves it necessary.

## Sessions

S02 ships an AEAD-encrypted cookie store; S05 extends it to carry `user_id`. `Session.IsAnonymous()` reports whether a user is bound. The cookie store re-encrypts on every Save, producing a fresh ciphertext — that's our defense against session fixation.

Login flow:

1. Verify password.
2. Mutate the loaded session to set `UserID` and `IssuedAt`.
3. Call `SessionStore.Save` — re-encrypts the cookie under the new state.

Logout flow:

1. `SessionStore.Clear` — deletes the cookie.

## Auth middleware

`internal/web/middleware/auth.go` adds:

- `OptionalUser(lookup)` — populates `CurrentUser{ID, Username}` into context when the session has a `user_id`. Anonymous requests still pass through.
- `RequireUser` — redirects to `/login?next=<requested-path>` for anonymous requests.
- `MaxBodySize(n)` — wraps `r.Body` in `http.MaxBytesReader(w, r.Body, n)`. Used on auth POSTs (4 KiB).

`CurrentUserFromContext(ctx)` returns the bound `CurrentUser` (zero value when anonymous).

## CSRF

S02's `nosurf` wrapper guards every state-changing route. S05 fixes a wart: nosurf's default `isTLS` returns true unconditionally, which makes its same-origin Referer check require an `https` scheme even on plain-HTTP requests. We set `isTLS` to a function that consults `r.TLS` and `X-Forwarded-Proto: https` so dev (HTTP) and prod (TLS-terminated) both work correctly.

## Signup → verify → login → logout

End-to-end flow exercised in `internal/web/handlers/auth/auth_test.go::TestSignup_Verify_Login_Logout`:

1. `GET /signup` — render form. CSRF cookie set by nosurf.
2. `POST /signup` — validate username (whitelist + reserved-name list), email shape, password length, common-password check. Hash password. In one transaction: create user, create user_email (unverified), set `users.primary_email_id`, create email_verifications row. Send verification email (best-effort — SMTP failure does not break signup). Redirect to `/login?notice=signup-pending`.
3. `GET /verify-email/{token}` — hash the token, look up the row, validate (not used, not expired). In one transaction: mark the email verified, flip `users.email_verified` if it's the primary, mark the verification row `used_at`. Redirect to `/login?notice=verified`.
4. `POST /login` — load user (constant-time fallback if missing), Verify password, check suspended/verified flags, reset throttle counter, touch `last_login_at`, mutate session with `user_id`, Save (re-encrypts cookie), redirect to `/` (or `?next=` target if it's a relative path).
5. `POST /logout` — Clear session. Redirect to `/login?notice=logged-out`.

## Password reset

`POST /password/reset` always responds with the same generic notice — "If an account is registered to that address, we've sent a password-reset link." — whether or not the email exists. No email is sent for unknown addresses. This is the canonical defense against enumeration via the reset flow.

`POST /password/reset/{token}` validates the token (lookup by sha256, not used, not expired), enforces the password policy (length + common-password), hashes via argon2id, updates `users.password_hash`/`password_algo`/`password_updated_at` and marks the reset row consumed — atomically in one transaction. Redirects to `/login?notice=password-reset`.

## Honeypot

The signup form has a hidden `company` field positioned off-screen with `tabindex="-1"`. Bots tend to fill every field they see; humans don't. Non-empty submissions are silently treated as success (303 redirect to the same `/login?notice=signup-pending` page) so bots can't detect the trap.

## Admin escape hatch

```sh
shithubd admin reset-password <username>
```

Generates a fresh password-reset token (1-hour TTL), persists it, and emails the link to the user's primary email via the configured backend. Useful when a locked-out user can't drive the public reset flow themselves.

## Configuration

All auth settings flow through `internal/infra/config` (see `docs/internal/config.md`):

| Key | Type | Default | Notes |
|---|---|---|---|
| `auth.require_email_verification` | bool | `true` | When true, login is rejected until the primary email is verified. |
| `auth.base_url` | string | `http://127.0.0.1:8080` | Used for absolute links in emails. |
| `auth.site_name` | string | `shithub` | Branding token for email subjects/bodies. |
| `auth.email_from` | string | `shithub <noreply@shithub.local>` | Envelope From for outgoing email. |
| `auth.email_backend` | string | `stdout` | `stdout | smtp | postmark | resend`. |
| `auth.smtp.addr` | string | `127.0.0.1:1025` | Required when `email_backend=smtp`. |
| `auth.smtp.username` | string | `""` | Optional. |
| `auth.smtp.password` | string | `""` | Optional. Redacted by `config print`. |
| `auth.postmark.server_token` | string | `""` | Required when `email_backend=postmark`. Redacted. |
| `auth.resend.api_key` | string | `""` | Required when `email_backend=resend`. Redacted. |
| `auth.argon2.memory_kib` | uint32 | `65536` | argon2id memory cost (KiB). |
| `auth.argon2.time` | uint32 | `3` | argon2id iterations. |
| `auth.argon2.threads` | uint8 | `2` | argon2id lanes. |

## Testing

- **Unit tests** (no DB): `internal/auth/password`, `internal/auth/token`, `internal/auth/email`, `internal/auth/reserved`, `internal/passwords`. Run with `go test ./...`.
- **DB-backed tests** (skip when `SHITHUB_TEST_DATABASE_URL` is unset): `internal/auth/throttle`, `internal/web/handlers/auth`. Use the dbtest harness (clone-from-template) for parallel safety.

The auth integration tests cover:
- Signup → verify → login → logout
- Password reset end-to-end
- Generic notice for unknown email at reset time (no enumeration)
- Login throttled after 6 failed attempts (with `Retry-After`)
- Login response time roughly equal between existing and missing usernames
- Reserved-name rejection
- Common-password rejection
- Honeypot silently accepted

## Pitfalls / what to remember

- **Don't leak existence via timing.** Always call `password.VerifyAgainstDummy` on missing-user login attempts.
- **Don't leak existence via reset.** Always render the same notice regardless of whether the email maps to a real account.
- **Username uniqueness** is enforced by the `citext UNIQUE` constraint, not by a pre-check (TOCTOU race otherwise).
- **No password length cap** at the application layer — but cap the request body so the argon2 hasher can't be weaponized.
- **`html/template` HTML-escapes attribute values** including `+` → `&#43;`. Browsers decode this transparently; non-browser clients (e.g. test clients) need to call `html.UnescapeString`.
- **Reserved-name list is load-bearing** — when adding a new top-level route, update `internal/auth/reserved.go` in the same PR.
- **Email send failure must not break signup.** The verify-resend flow handles delivery retries.
