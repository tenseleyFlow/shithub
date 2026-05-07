# Two-factor authentication

S06 ships TOTP-based 2FA: enrollment, login challenge, recovery codes, encrypted-at-rest secrets, audit-log writes, and an admin escape hatch. WebAuthn and SMS are intentionally out of scope (WebAuthn is its own future sprint; SMS is phishable and we won't ship it).

## What's wired

- `user_totp` (one row per user, encrypted secret + nonce + last-used counter).
- `user_recovery_codes` (10 per enrollment, sha256-stored, single-use).
- `auth_audit_log` (generic schema reused by future sprints).
- `internal/auth/secretbox` — chacha20poly1305 wrapper, 32-byte key from config.
- `internal/auth/totp` — secret/URI/Verify/recovery codes/SVG QR.
- `internal/auth/audit` — typed action recorder.
- `/login/2fa` challenge step.
- `/settings/security/2fa/{enable,disable,regenerate}`.
- `shithubd admin clear-2fa <username>` operator escape hatch.

## Login flow with 2FA enrolled

```
POST /login (password)
   │
   ├─ password OK, no 2FA confirmed → set Session.UserID, redirect /
   │
   └─ password OK, 2FA confirmed → set Session.Pre2FAUserID,
                                   redirect /login/2fa (?next=...)
                                       │
                                       ▼
                                  POST /login/2fa (TOTP or recovery code)
                                       │
                                       ├─ accepted → drop Pre2FAUserID,
                                       │             set UserID, save (re-issued cookie)
                                       │             redirect to next or /
                                       │
                                       └─ rejected → form re-rendered;
                                                     after 5 wrong tries / 5 min,
                                                     pre-2FA marker is dropped
                                                     and user must restart.
```

`Session.Pre2FAUserID` is the marker that says "password OK, 2FA pending." It carries no privileges — handlers that check `CurrentUser.IsAnonymous()` still see this as anonymous because `UserID == 0`. Only the 2FA challenge handler reads `Pre2FAUserID`.

## Enrollment flow

```
GET /settings/security/2fa/enable
   │  Server generates 20-byte secret. Encrypts with AEAD. Stores
   │  user_totp row with confirmed_at NULL. Renders QR SVG inline.
   ▼
POST /settings/security/2fa/enable (current TOTP code)
   │  Decrypt stored secret. Verify code with ±1 step skew.
   │  Atomically:
   │    - ConfirmUserTOTP (sets confirmed_at, last_used_counter).
   │    - DeleteUserRecoveryCodes (defensive — clears any leftover codes).
   │    - InsertRecoveryCode × 10.
   │    - audit.Action2FAEnabled, audit.ActionRecoveryCodesIssued.
   │  Render recovery codes ONCE.
   │  notifyUser("2fa_enabled") best-effort.
   ▼
User saves the codes. From this point /login goes through /login/2fa.
```

Race protection: `ConfirmUserTOTP` is `UPDATE ... WHERE confirmed_at IS NULL`. If two concurrent confirms arrive, exactly one wins (rows-affected==1); the loser falls through to a 303 to `/disable`.

## Disable / regenerate flow

Both require password + current TOTP. This is non-negotiable: a hijacked session must not be able to disable 2FA without proving knowledge of the password AND the authenticator. The `confirmPasswordAndTOTP` helper enforces this in one place.

- **Disable** clears `user_totp`, clears recovery codes, audits `2fa_disabled`, sends a notification email.
- **Regenerate** keeps the TOTP row, deletes + re-issues recovery codes, audits `recovery_codes_regenerated`, sends a notification email. Used when the user has saved their codes somewhere they no longer trust.

## Counter anti-replay

`last_used_counter` stores the highest TOTP step we've accepted for the user. On every successful Verify, the handler calls `BumpTOTPCounter` which atomically:

```sql
UPDATE user_totp
SET last_used_counter = $2
WHERE user_id = $1 AND $2::bigint > last_used_counter
```

If rows-affected == 0, the code is from a step ≤ the stored counter — i.e. a replay — and the handler rejects it. Combined with the 30-second TOTP period, this caps the replay window at the `Verify` skew tolerance (±1 step → 30 seconds either side).

The integration test `TestTwoFactor_CounterReplayRejected` exercises this: enroll, log in once with code from step T, log out, log in again with the SAME code — expects rejection.

## Recovery codes

Format: `XXXX-XXXX-XXXX` using a reduced alphabet (`ACDEFGHJKMNPQRTUVWXYZ234`) to avoid easily-confused glyphs (no `0/O`, no `1/I/L`, no `8/B`, no `S`).

- Generated 10 at a time at enrollment or regenerate.
- Stored as sha256 of the normalized form (uppercase, no dashes/spaces).
- Single-use via `used_at`.
- Shown to the user **once** at generation time. Handlers must not store them in the session.

The login-challenge handler tells codes apart by shape (`LooksLikeRecoveryCode` checks length + alphabet). 6-digit numerics route to `verifyTOTPCode`; everything else routes to `consumeRecoveryCode`.

## Encryption at rest

`internal/auth/secretbox` wraps `chacha20poly1305`. Every TOTP row stores `(secret_encrypted, secret_nonce)`. The nonce is 12 random bytes per row — well below the birthday bound at our scale.

The 32-byte key comes from config:

```toml
[auth]
totp_key_b64 = "<base64-encoded 32 bytes>"
```

Or env: `SHITHUB_AUTH__TOTP_KEY_B64=...` (also accepts the alias `SHITHUB_TOTP_KEY` for symmetry with `SHITHUB_SESSION_KEY`).

If the key isn't set, the `/settings/security/2fa/*` routes are NOT registered — startup logs a warning. Login through `/login/2fa` still works for users who haven't enrolled (they never go through that path).

### Key rotation

Rotating `auth.totp_key_b64` without re-encrypting every row breaks every existing 2FA login. The rotation procedure:

1. Generate the new key: `openssl rand -base64 32`.
2. Run a one-off migration that decrypts each row with the OLD key and re-seals with the NEW key. The migration must complete inside a single deploy window.
3. Swap `auth.totp_key_b64` to the new value.
4. Restart shithubd.

A future sprint will ship this as a `shithubd admin rotate-totp-key` command. Until then, treat the key as effectively permanent.

## Audit log

`auth_audit_log` is intentionally generic. Every 2FA state change writes a row:

| Action | Who writes | Notes |
|---|---|---|
| `2fa_enabled` | enroll handler | meta: `{}` |
| `recovery_codes_issued` | enroll + regen | meta: `{count: 10}` |
| `recovery_code_used` | challenge handler | meta: `{}` |
| `recovery_codes_regenerated` | regen handler | meta: `{count: 10}` |
| `2fa_disabled` | disable handler | meta: `{}` |
| `admin_cleared_2fa` | `shithubd admin clear-2fa` | meta: `{admin: "cli"}` |

Future sprints will reuse the same table (S07 SSH key changes, S15 permissions, S30 org membership, S34 admin actions). When adding a new action, append a constant in `internal/auth/audit/audit.go` and document it here.

Audit retention: indefinite for security-relevant events. Revisit at S37.

## Admin escape hatch

```sh
shithubd admin clear-2fa <username>
```

For support cases where the user has lost both their authenticator AND their recovery codes. Wipes `user_totp` + `user_recovery_codes`, writes `admin_cleared_2fa` to the audit log, and emails the user a notification (best-effort). The user must re-enroll 2FA after this.

By policy this should only be invoked after manual identity verification through a support channel — there's no automated bypass.

## QR rendering

Server-side SVG. The otpauth URI is encoded via `boombuler/barcode` and emitted as a sequence of `<rect>` elements inside an `<svg>`. No client-side QR library, so no third-party JS sees the secret.

The otpauth URI carries the secret in plaintext. Defense in depth:

- The slog redactor (`internal/infra/log/log.go`) scrubs strings containing `otpauth://`.
- The SVG is generated server-side and never persisted.
- `TestQRSVG_Renders` asserts the rendered SVG does NOT contain the secret's base32 form (only the encoded QR modules).

## Rate limiting

The 2FA challenge step uses the existing `auth_throttle` table:

| Scope | Identifier | Max | Window |
|---|---|---|---|
| `2fa` | `ip:<client-ip>\|uid:<user-id>` | 5 | 5 minutes |

After 5 failed attempts, the pre-2FA marker is dropped and the user has to start over from `/login`.

## Security properties

- **Password reset does NOT bypass 2FA.** After a reset, the user is still anonymous (or pre-2FA). They must complete the challenge to get a full session.
- **Disable requires password + TOTP.** A hijacked session can't strip 2FA.
- **TOTP secrets are encrypted at rest.** A DB dump alone can't generate codes.
- **Counter anti-replay** caps a stolen code's reuse window at ±30s of when it was first burned.
- **Audit log** preserves enable/disable/regenerate/clear events for forensic review.
- **otpauth URI never logged.** Slog redactor + handler discipline.

## Testing

- **Unit tests** (no DB): `internal/auth/totp`, `internal/auth/secretbox`. Run with `go test ./...`.
- **DB-backed tests** (skip when `SHITHUB_TEST_DATABASE_URL` is unset): `internal/auth/audit`, `internal/web/handlers/auth`.

Integration tests in `internal/web/handlers/auth/twofactor_test.go`:

- `TestTwoFactor_Enroll_Logout_Login_Challenge_FullSession`: full happy path.
- `TestTwoFactor_RecoveryCode_OneTimeUse`: recovery code consumed, second use rejected.
- `TestTwoFactor_CounterReplayRejected`: same TOTP step rejected on second use.
- `TestTwoFactor_DisableRequiresPasswordAndTOTP`: wrong creds rejected; correct creds succeed.

## Pitfalls / what to remember

- **Don't show recovery codes more than once.** The recovery template only renders the codes when `RecoveryCodes` is set on the page data; handlers set it exactly once at generation time.
- **Don't bypass 2FA on password reset.** Post-reset flow must still go through `/login/2fa` if the user has 2FA enrolled. Reset just changes `users.password_hash` — `user_totp.confirmed_at` is untouched.
- **Don't put secrets in `meta` JSON.** Audit rows can leak via DB-dump or admin queries.
- **Authenticator clock drift** is bounded at ±30s by the SkewSteps=1 setting. Document this in user help when the settings page lands (S10).
- **Lost authenticator + lost recovery codes = locked out.** That's by design. Admin escape hatch is the only recovery; treat it as a high-friction, identity-verified path.

## Related docs

- `docs/internal/auth.md` — email/password auth (S05).
- `docs/internal/config.md` — full config reference (incl. `auth.totp_key_b64`).
- `docs/internal/observability.md` — slog redaction (otpauth scrub).
