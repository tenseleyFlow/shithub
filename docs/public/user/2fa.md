# Two-factor authentication

Two-factor authentication (2FA) requires a second proof — beyond
your password — before you can sign in. shithub supports
**TOTP** (Time-based One-Time Password): six-digit codes from an
authenticator app that change every 30 seconds.

Strongly recommended. Account takeover is the most common bad
outcome on a forge; a stolen password by itself can't sign in if
2FA is on.

## Setting up TOTP

1. Settings → Account security → Two-factor authentication →
   "Enable TOTP".
2. Open your authenticator (Google Authenticator, 1Password,
   Authy, etc.) and scan the QR code. The text-form secret is
   shown beneath the QR if your app needs it.
3. Enter the six-digit code from the app to confirm enrollment.
4. **Save the recovery codes** that appear. There are 10. Each is
   single-use. Store them somewhere your authenticator-device
   loss won't take with it (password manager, paper safe).

You're now enrolled. The next sign-in will ask for the code after
the password.

## Recovery codes

If you lose your authenticator (phone died, factory reset, etc.),
recovery codes are your way back in.

- Each code works **once**.
- Used codes are crossed off — you can see which are spent.
- When you have ≤2 unused codes left, the UI nudges you to
  regenerate. Regenerating invalidates all previous codes.

If you exhaust all 10 codes, the only path back in is operator
intervention — they cannot give you your codes back, but they can
disable 2FA on the account after verifying your identity through
a side channel.

## Disabling 2FA

Settings → Account security → "Disable TOTP". Requires entering
the current TOTP code.

This is recorded in your audit log. If you didn't disable 2FA,
treat that audit row as evidence of compromise and rotate
everything (password, all PATs, all SSH keys).

## Sign-in flow with 2FA

1. Username + password.
2. If correct, you're prompted for the six-digit code (or "use a
   recovery code").
3. Code accepted → signed in.

## What 2FA does and doesn't protect

- **Protects against** stolen passwords (phishing, leaked-DB
  reuse, shoulder-surfing).
- **Does not protect against** stolen sessions — once you're
  signed in, the session cookie is the access. Use "Sign out
  everywhere" if a device is lost.
- **Does not protect** PAT-based git or API access — PATs are
  separate credentials; rotate them on the same cadence as
  passwords.
