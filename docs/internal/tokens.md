# Personal access tokens

S08 ships personal access tokens (PATs) — the authentication primitive for shithub's HTTP API and (eventually) git-over-HTTPS pushes. Tokens are minted via `/settings/tokens`, displayed once, and revocable from the same page.

## What's wired

- `user_tokens` table with global `token_hash UNIQUE` + scope array.
- `internal/auth/pat` package: minting, sha256 hashing, scope set, in-memory last-used debouncer.
- `internal/web/middleware` PATAuth + RequireScope middlewares.
- `internal/web/handlers/api` package with `GET /api/v1/user` (PAT-only).
- `/settings/tokens` (list, create one-time-display, revoke).
- Recent-auth gate (Session.Recent2FAAt within 10 min) when 2FA is enrolled.
- token_created notification email.
- Authorization-header redaction + URL-credential stripping in slog.

## Token format

`shithub_pat_<32-char-base62>`, e.g. `shithub_pat_F75zxAWXLBWR3mno8hCa2eBM8p4X5saw`.

The fixed `shithub_pat_` prefix is intentional:

- Secret-scanning vendors (GitHub Advanced Security, GitGuardian, etc.) recognize this exact pattern and can flag it in source code or commit history.
- Our slog redactor scrubs any string containing the prefix.
- The prefix is stripped+redacted in URL-credential form (`https://user:shithub_pat_…@host`) so even an accidentally-pasted git remote stays safe in logs.

The 32-char payload carries ~190 bits of entropy (`log2(62)*32`) — well beyond brute-force budgets.

## Storage

Only the sha256 hash of the raw token lands in the DB:

| Column | Type | Notes |
|---|---|---|
| `token_hash` | `bytea` | sha256 of raw, UNIQUE — auth lookup uses an index seek |
| `token_prefix` | `text` | first ~16 chars of raw (incl. `shithub_pat_`), display-only |
| `scopes` | `text[]` | normalized via `pat.NormalizeScopes` before INSERT |
| `expires_at` | `timestamptz` NULL | NULL = no expiry; UI defaults to 90 days |
| `last_used_at`, `last_used_ip` | best-effort, debounced | |
| `revoked_at` | `timestamptz` NULL | Once set, the token is dead even if not yet expired |

sha256 without salt is acceptable here because the input is itself uniform-random — there is no rainbow-table surface. Constant-time compare is provided by `pat.EqualHash` and used by callers that compare hashes outside a DB lookup.

## Scopes

Coarse, GitHub-classic-PAT-style:

| Scope | Grants |
|---|---|
| `repo:read` | Read access to private repos the user can access |
| `repo:write` | Push, branch, PR creation. Implies `repo:read`. |
| `user:read` | Read user profile, verified emails |
| `user:write` | Modify user settings (NOT password / 2FA / tokens — session-only ops). Implies `user:read`. |
| `admin:read` | Read admin endpoints (only honored for users with admin role) |

Implication is enforced in `pat.HasScope` so callers don't have to spell it out.

A route declares its required scope via `middleware.RequireScope(pat.ScopeUserRead)`. **Pure-session callers (no Authorization header) bypass scope checks entirely** — sessions have implicit full scope.

## Authentication paths

The middleware accepts three forms, all routed to the same lookup:

1. `Authorization: token <pat>` (preferred)
2. `Authorization: Bearer <pat>`
3. HTTP Basic where the password is the PAT (matches `git`'s credential helpers)

```sh
# Preferred
curl -H 'Authorization: token shithub_pat_…' https://shithub/api/v1/user

# Bearer (RFC 6750)
curl -H 'Authorization: Bearer shithub_pat_…' https://shithub/api/v1/user

# git-over-HTTPS (works because git uses Basic with password=<pat>)
git push https://alice:shithub_pat_…@shithub/owner/repo.git main
```

On any auth failure the middleware writes a 401 with a `WWW-Authenticate: Bearer realm="shithub", error="invalid_token", error_description="<reason>"` header. Reasons we emit:

- `invalid token` — missing, malformed, unknown, or any unrecoverable error
- `token revoked`
- `token expired`
- `account suspended`

We don't distinguish "unknown" from "malformed" in the response — leaking that distinction would let an attacker probe whether a particular hash exists.

## Recent-auth gate

Creating a PAT requires a recent successful TOTP challenge if the user has 2FA enrolled:

- `Session.Recent2FAAt` is set on a successful `/login/2fa` and on the enrollment-confirm path.
- `recentAuthOK` checks the timestamp is within 10 minutes (`recentAuthWindow`).
- Users without 2FA are treated as recent (the gate only matters when 2FA is in play).

When the gate fails, the token-create form shows a flash directing the user to sign in again with their authenticator code. This raises the bar on stolen-cookie attacks issuing tokens.

## One-time display

Raw tokens are shown to the user **exactly once** at create time, then discarded. The handler:

1. Mints the raw + hash + display prefix.
2. Inserts the row with hash + prefix + scopes.
3. Renders the listing template with `JustCreatedRaw` set to the raw value.

The template displays `JustCreatedRaw` in a `<pre><code>` block with a clear "save this now — won't be shown again" message. The raw value never lands in the session, the URL, or any log line.

## Last-used debouncing

Every authenticated request would otherwise burn an UPDATE on `user_tokens`. To avoid hot-row contention, the middleware uses an in-memory debouncer (`pat.Debouncer`) keyed by `token_id` with a 60-second window:

- First request in a window → `ShouldTouch` returns true → goroutine writes `last_used_at` / `last_used_ip` with a 500ms timeout.
- Subsequent requests within the window → `ShouldTouch` returns false → no write.

Trade-offs:

- **Lost on process restart.** Acceptable — `last_used_at` is a UI-display field, not an audit primitive.
- **Per-process.** Multi-replica deployments will have ~N writes per token per window where N = replicas. Still bounded.
- **Goroutine detached from `r.Context()`.** Intentional — a debounced touch is best-effort; we'd rather complete it than drop on client disconnect (`G118` is suppressed with that justification).

`pat.Debouncer.Forget(tokenID)` is exposed for revoke-time invalidation in future sprints.

## Auto-revoke triggers

- **User suspension** → all active tokens revoked via `RevokeAllUserTokens`.
- **User deletion** (post-grace) → tokens hard-deleted via the `ON DELETE CASCADE` from `user_id`.
- **Password change** → tokens are NOT revoked by default. Matches GitHub's behavior; user-configurable preference lands in S10.

## Logging discipline

Tokens leak in three classic places: request logs, error reporters, and panic dumps. Defenses:

1. `Authorization` is in the slog redactor's secret-attr list — value `***`.
2. `shithub_pat_` is in the value-marker list — any string containing it collapses to `***`.
3. URL credentials of the form `scheme://user:pass@host` get the userinfo stripped via regex while preserving host + path so logs stay useful (e.g. `postgres://***@127.0.0.1:5432/shithub`).
4. `errrep.SlogHandler` flows error-level records through the same redactor before forwarding to GlitchTip.

Negative tests cover #1, #2, and #3 in `internal/infra/log/log_test.go`.

## Settings UI

`/settings/tokens` shows:

- Active and revoked tokens (revoked sorted last) with name, prefix, scopes, last-used timestamp, expiry.
- A create form with name, expiry picker (30/90/365/none), and scope checkboxes.
- A flash banner when the recent-auth gate is failing.
- The newly-minted raw token in a one-time-display block at the top of the page when `JustCreatedRaw` is set.

## Operational notes

- **Per-user cap:** 50 tokens (`pat.MaxTokensPerUser`). Configurable via constant; the listing handler enforces it on create.
- **Audit:** `pat_created` and `pat_revoked` rows in `auth_audit_log` carry `{token_id, prefix, scopes}` (creation) and `{token_id}` (revocation). Never the raw token, never the hash.
- **Notification email** on every successful create with name + prefix + IP.

## API surface (S08 baseline)

- `GET /api/v1/user` — returns the authenticated user's public profile JSON. Requires `user:read` (or `user:write` via implication).

Future sprints add:

- `GET /api/v1/repos/{owner}/{repo}` (S11 unblocks)
- `POST /api/v1/repos` (S11)
- PR / issue endpoints (S22, S23)

## Pitfalls / what to remember

- **Don't store the raw token anywhere.** The DB has the hash; the handler shows the raw exactly once.
- **Don't compare hashes with `bytes.Equal`.** Use `pat.EqualHash` (constant-time).
- **Don't put scopes on the request — put them on the token.** Sessions have implicit full scope; PATs carry exactly what the user picked.
- **Don't bypass the recent-auth gate.** A hijacked session must NOT be able to mint a PAT without a fresh TOTP step.
- **Don't log `Authorization` headers.** The redactor catches it, but a custom logger that bypasses our handler doesn't get the redaction for free.
- **Don't change a token's scopes in place.** "Edit scopes" = "revoke + create new" UX. We never silently widen a token's privileges.

## Related docs

- `docs/internal/auth.md` — email/password auth (S05).
- `docs/internal/2fa.md` — TOTP + recovery codes (S06), recent-auth pattern.
- `docs/internal/ssh-deploy.md` — git-over-SSH (S07); PAT covers git-over-HTTPS in S12.
- `docs/internal/observability.md` — slog redaction.
