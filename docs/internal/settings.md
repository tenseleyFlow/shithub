# User settings

S10 ships every page under `/settings/*` for the authenticated user — the public profile editor, avatar upload pipeline, account-management surface (username + password), email management, notification preferences, theme, current-session view + log-out-everywhere, and the danger zone (soft-delete with grace window). State-changing actions write audit-log rows and send notification emails (gated by user prefs except for security alerts).

## Routes

All routes live under `/settings` and require an authenticated user (chi `RequireUser` middleware on the group).

| Route | Method | Source | Notes |
|---|---|---|---|
| `/settings/profile` | GET/POST | `auth.profile.go` | Display name, bio, location, website, company, pronouns. Right-side avatar card. |
| `/settings/profile/avatar` | POST | `auth.avatar.go` | Multipart upload. 5 MiB cap, EXIF stripped, 460/200/40 PNG variants. |
| `/settings/profile/avatar/remove` | POST | `auth.avatar.go` | Clears `users.avatar_object_key`; objects are kept (orphan sweep is post-MVP). |
| `/settings/account` | GET | `auth.account.go` | Username change form. |
| `/settings/account/username` | POST | `auth.account.go` | Inserts `username_redirects`, renames `users`, audit + notify. |
| `/settings/password` | GET/POST | `auth.password_change.go` | Current pw + 2FA gate, bumps `session_epoch`. |
| `/settings/emails` | GET/POST | `auth.emails.go` | List and add a new address (sends a verify email). |
| `/settings/emails/{id}/resend` | POST | `auth.emails.go` | Re-issues a verification token for an unverified row. |
| `/settings/emails/{id}/primary` | POST | `auth.emails.go` | Atomically swaps the primary; refuses unverified rows. |
| `/settings/emails/{id}/remove` | POST | `auth.emails.go` | Refuses to delete the primary address. |
| `/settings/appearance` | GET/POST | `auth.appearance.go` | Theme picker. Writes both `users.theme` and the `theme` cookie. |
| `/settings/notifications` | GET/POST | `auth.notifications.go` | Generic k/v editor over `user_notification_prefs`. |
| `/settings/sessions` | GET | `auth.sessions.go` | Current session info. |
| `/settings/sessions/logout-everywhere` | POST | `auth.sessions.go` | Bumps `users.session_epoch`. |
| `/settings/security/2fa/*` | GET/POST | `auth.twofactor.go` | (Wired in S06.) |
| `/settings/keys` and `/settings/keys/{id}/delete` | GET/POST | `auth.sshkeys.go` | (Wired in S07.) |
| `/settings/tokens`, `/settings/tokens/{id}/revoke` | GET/POST | `auth.tokens.go` | (Wired in S08.) |
| `/settings/danger` | GET/POST | `auth.danger.go` | Soft-delete with 14-day grace; restore-on-login. |

The settings sidebar (`internal/web/templates/_settings_nav.html`) is shared by every settings template. Each handler passes a `SettingsActive` string so the active link is highlighted.

## Session epoch — log-out-everywhere

`users.session_epoch` (integer, default 0) is bumped to invalidate every cookie that carries an older value.

- The login handler snapshots the user's current epoch into `Session.Epoch` at issue time.
- `middleware.OptionalUser`'s `UserLookup` returns `(username, epoch)`; when the session's stored epoch ≠ the DB's, the binding is skipped and `RequireUser` will redirect the next protected hit to `/login`.
- Three handlers bump:
  - **Password change** (`/settings/password`) — security best practice; explicitly mentioned in the success flash.
  - **Log out everywhere** (`/settings/sessions/logout-everywhere`) — the explicit user-driven action.
  - **Account deletion** (`/settings/danger`) — kicks every live session of the soon-to-be-deleted user.
- After every bump, the *current* session is re-issued with the new epoch so the user staying on this browser is not signed out.

The cookie itself is not actively cleared on bump (other than for account deletion). The stale cookie is harmless: its next protected hit lands on `/login`. It also expires via its `MaxAge` window (`session.DefaultMaxAge`, currently 30 days).

## Username change — rate limit + redirect

Three rules:

1. **Shape** — `^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`. Input is lowercased server-side, so users who type `Bob` get `bob`.
2. **Reservation** — `internal/auth/reserved.IsReserved`.
3. **Cap** — at most 3 changes per rolling 60-day window. Counted by `username_redirects.changed_at` rows for that user, so the redirect IS the audit trail; no separate counter table.

Tx body:

```sql
INSERT INTO username_redirects (old_username, user_id) VALUES ($1, $2);
UPDATE users SET username = $3 WHERE id = $2;
```

Post-commit: `audit_log` row + state-change email. The new name is unique by `users_username_unique` (citext); a redirect row clash on the same name (someone else's old name during their grace window) raises 23505 and surfaces "That username is taken."

The old name is reserved as a redirect for 30 days (handled by the existing username-redirects logic introduced in S09). The 30-day reservation is implicit — `signup` already consults the same table.

## Avatar pipeline

`internal/avatars.Process(io.Reader)`:

1. Reads up to `MaxUploadBytes + 1` (5 MiB + sentinel) and rejects oversized payloads.
2. `image.DecodeConfig` first — checks the format whitelist (PNG / JPEG / GIF) AND rejects images whose pixel area exceeds `MaxPixelArea` (24 megapixels) before allocating the full pixel buffer. Defends against decompression-bomb files that are tiny on disk but enormous when decoded.
3. Decode for real.
4. Center-square crop so the resize never squashes.
5. CatmullRom resize via `golang.org/x/image/draw` to each `VariantSizes` target (460 / 200 / 40).
6. Re-encode each as PNG (which strips any EXIF the source carried — we never read it).
7. Hash the largest variant; return that hash for the storage key.

Object-store layout: `avatars/{user_id}/{hash}.png` for the largest variant; smaller variants get a `-{size}` suffix. The largest variant's key is stored in `users.avatar_object_key` and the public `/avatars/{username}` route serves it directly.

Variants 200 and 40 are written today but not yet served — a future sprint can add `/avatars/{username}/{size}` without re-resizing.

## Email management

The 4-action surface:

- **Add** — creates a `user_emails` row with `is_primary=false, verified=false` and a new `email_verifications` row. The verification email is sent post-commit.
- **Resend** — only for unverified rows; rotates the token + sets a fresh expiry.
- **Set primary** — refuses unverified rows. Atomic swap is `SetUserEmailPrimary` (`UPDATE … SET is_primary = (id = $2) WHERE user_id = $1`); the partial unique index `user_emails_one_primary_per_user` keeps the invariant.
- **Remove** — refuses the primary address (DB query has `AND is_primary = false`); user must promote a different verified address first.

A primary email change writes an `audit_log` row (TODO: explicitly — currently only the email goes out) and sends a `primary_email_changed` notice on the `account_changes` channel.

## Notification preferences

`user_notification_prefs` is a tiny key/value/jsonb table. The settings page surfaces a fixed list of "channels" (`internal/web/handlers/auth/notifications.go::notifChannels`):

| Key | Default | Toggleable | Carries |
|---|---|---|---|
| `security_alerts` | on | no (Required) | password_changed, log_out_everywhere, 2FA changes |
| `account_changes` | on | yes | username_changed, primary_email_changed, account_deletion_initiated |
| `product_news` | off | yes | reserved for future broadcast emails |

Storage strategy: when the desired state matches the channel's default, the row is **deleted** rather than upserted. This keeps the table small and makes "reset to defaults" a no-op. The form omits a checkbox to encode "off."

`shouldNotify(ctx, userID, kind)` is the single gate: kinds whose channel is `security_alerts` always return true; kinds on `account_changes` consult the persisted row, falling back to the default when absent.

## Soft-delete + 14-day grace

`/settings/danger` requires the user to retype their username and current password. On confirmation:

1. `SoftDeleteUser` — `UPDATE users SET deleted_at = now() WHERE id = $1`.
2. `BumpUserSessionEpoch` — invalidates every cookie.
3. (post-commit) Audit row + notification email (built directly via `email.NoticeMessage` with the captured primary address — `notifyState`'s usual lookup filters out soft-deleted users).
4. `SessionStore.Clear(w)` and a 303 to `/?notice=account-deleted`.

Restore-on-login (in the login handler):

```go
user, err := h.q.GetUserByUsernameIncludingDeleted(ctx, pool, username)  // see deleted rows
if user.DeletedAt.Valid && time.Since(user.DeletedAt.Time) >= deletionGraceWindow {
    // Past the 14-day window — treat as nonexistent.
    password.VerifyAgainstDummy(pw)
    render("Incorrect username or password.")
    return
}
// Verify password; if OK, RestoreUserAccount() (sets deleted_at = NULL).
```

A within-grace user therefore restores the moment they prove ownership of the password. Past the window the row stays soft-deleted and the response is indistinguishable from "doesn't exist," same constant-time cost.

The public profile (`/{username}`) still uses `GetUserByUsername` (which filters `deleted_at IS NULL`), so a soft-deleted account renders as 404 immediately. Suspended accounts still render the dedicated 410 "unavailable" page introduced in S09.

## State-change notifications — kinds

`internal/auth/email/messages.go::noticeBodies` plus `auth/notify.go::kindToChannel`:

| Kind | Channel | Trigger |
|---|---|---|
| `password_changed` | security_alerts | POST /settings/password success |
| `log_out_everywhere` | security_alerts | POST /settings/sessions/logout-everywhere |
| `username_changed` | account_changes | POST /settings/account/username success |
| `primary_email_changed` | account_changes | POST /settings/emails/{id}/primary |
| `account_deletion_initiated` | account_changes | POST /settings/danger success |
| `2fa_enabled` / `2fa_disabled` / `recovery_regenerated` / `admin_cleared_2fa` | security_alerts | (Wired in S06.) |

Send is best-effort: failures are logged but never break the flow.

## Theme + first-paint flash avoidance

Set on POST `/settings/appearance`:

- `users.theme` — source of truth across devices.
- `theme` cookie (`SameSite=Lax`, `MaxAge` 1 year, NOT `HttpOnly` — the `_layout.html` inline script reads it before any CSS computes).

Allowed values: `""` (the cookie wins, defaulting to system preference), `light`, `dark`, `auto`, `high_contrast`. Mirrors the CHECK constraint added in migration 0016. Empty value clears the cookie.

## Reference visual conformance

Every settings page renders inside `_settings_nav.html` (left sidebar with "Settings" group, "Access" group, "Danger" group) + a content pane of card-style sections. Visual reference: GitHub's own settings pages (kept in `.refs/github/images/user/`). Notable adopted patterns:

- Sidebar grouped headers (`Access`, `Danger`) with uppercase letter-spaced labels.
- Active link with left-edge accent border + subtle canvas-subtle background.
- Card sections separated by hairline `--border-muted` borders.
- Danger-zone red-bordered card and red-text headline.
- Two-column inner layout for the profile editor (form left, profile-picture aside right).
- Pill chips for email row state (`primary` / `verified` / `unverified`).
- Theme picker as 5 radio cards in a responsive grid.

CSS lives in `internal/web/static/css/shithub.css` under `/* ----- settings shell (S10) ----- */` and the topic-specific blocks below it.

## Audit trail

Each security-relevant settings action records an `audit_log` row via `internal/auth/audit.Recorder`. Actions added in S10:

- `username_changed` (with `from`/`to` meta).
- `account_deleted` (with `grace_days` meta).
- `account_restored` (recorded by login restore-on-login when it fires).

The pre-existing `password_changed`, 2FA actions, and SSH/PAT actions are reused.

## Pitfalls / what to remember

- **Epoch bumps must re-issue the current session.** Otherwise the user signs themselves out. See `password_change.go::settingsPasswordSubmit` and `sessions.go::settingsSessionsLogoutAll` for the pattern.
- **Avatar route is read-mostly.** Uploaded variants are content-addressed by the largest variant's hash; the URL changes when the image changes, so `Cache-Control: public, max-age=31536000, immutable` on the avatar response is safe. Don't add a "no-cache" override.
- **Username regex is server-side normalization, not validation only.** "Bob" lowercases to "bob" before any other check; tests assume this.
- **Password change requires 2FA recency** when 2FA is enrolled. Users without 2FA bypass it (`recentAuthOK` returns true). Audit/notify still fire.
- **Soft-deleted users are invisible to `GetUserByUsername`**, so the post-deletion notification email is built using the captured primary address (`notifyDeletedAccount`) rather than the standard `notifyState` path.
- **Don't delete object-store keys on avatar removal.** Old objects accumulate; an orphan-sweep job is post-MVP.
- **Notification prefs schema is generic** (k/v/jsonb). Adding a channel is a code change to `notifChannels`, not a migration. Keep the channel list short.

## Related docs

- `docs/internal/auth.md` — login, sessions, recent-2FA gate.
- `docs/internal/2fa.md` — 2FA enrollment flow + secretbox.
- `docs/internal/profile.md` — public `/{username}` page (S09).
- `docs/internal/storage.md` — object-store conventions used by avatar uploads.
- `docs/internal/tokens.md` — `/settings/tokens` (S08).
