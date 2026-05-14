# Rotate secrets

Quarterly cadence; sooner if compromise is suspected. The secret
classes:

| Secret                       | Where it lives                               | Rotation procedure                        |
|------------------------------|----------------------------------------------|-------------------------------------------|
| `session.key_b64`            | `web.env`                                    | See "Session signing key" below.          |
| `auth.totp_key_b64`          | `web.env`                                    | See "TOTP AEAD key" below.                |
| Postgres `shithub` password  | `web.env` + `worker.env` + Postgres role     | See "DB password" below.                  |
| Postgres `shithub_hook` pwd  | sshd env + `hook-role-grants.sql` apply env  | See "DB password" below.                  |
| S3 access keys               | `web.env` + `worker.env` + Spaces dashboard  | See "Object store credentials" below.     |
| Postmark / SMTP creds        | `web.env`                                    | One-step: replace, redeploy.              |
| Webhook AEAD key             | per-row encrypted; key in `worker.env`       | Two-step migration, see below.            |
| Operator SSH keys            | `~operator/.ssh/authorized_keys` per host    | Add new key, verify, remove old.          |

## Session signing key

The session key signs the cookie that authenticates a logged-in
session. Rotating it logs **every user out** because every
existing cookie's MAC stops verifying.

1. Generate a new key:
   ```sh
   openssl rand -base64 32
   ```
2. Update the inventory variable `session_key`. Keep the old key
   in a comment for one rotation cycle so you can revert.
3. `make deploy ANSIBLE_INVENTORY=production ANSIBLE_TAGS=app`.
4. Verify: sign in to your own account with a fresh browser; the
   cookie set after sign-in is signed by the new key.

User-visible impact: every user is signed out. Notify in-band
before doing this if avoidable; do it without notice if the old
key may be compromised.

## TOTP AEAD key

The TOTP AEAD key encrypts every user's TOTP shared secret at
rest in the database. **Rotating this key requires a
re-encryption migration** — without it, every 2FA enrollment
becomes unreadable.

The procedure is:

1. Add the new key to `web.env` as `auth.totp_key_b64_next`
   alongside the existing `auth.totp_key_b64`.
2. Restart web (the package supports a "current + next" pair: it
   reads with current, falls back to next, writes with current).
3. Run the re-encryption job: `shithubd admin re-encrypt-totp
   --to-key=auth.totp_key_b64_next` (operator-only). This
   decrypts each row with the old key and re-encrypts with the
   new.
4. Promote `auth.totp_key_b64_next` to `auth.totp_key_b64` (drop
   the suffix), remove the old key.
5. Restart web.

Do not skip step 3. Failing to re-encrypt before retiring the old
key locks every 2FA-enabled user out of their account; recovery
codes are the only path back in, and not everyone has them
saved.

## DB password

Rotate by adding a new password and removing the old, **without
downtime**.

1. As `postgres`:
   ```sql
   ALTER ROLE shithub WITH PASSWORD '<new>';
   ```
2. Update `web.env` and `worker.env` `db_password`.
3. `make deploy ANSIBLE_INVENTORY=production ANSIBLE_TAGS=app`.
   The web/worker units will restart and reconnect with the new
   password.

If you suspect the old password was leaked, do steps 1–3 in
sequence within minutes — between (1) and (3) the running web
process still has its open connections (which authenticated
under the old password) but new connections will use the new.

## Object store credentials

1. In the Spaces dashboard, generate a new access key with the
   same scope as the old.
2. Update inventory `s3_access_key_id` and `s3_secret_access_key`.
3. `make deploy ANSIBLE_INVENTORY=production ANSIBLE_TAGS=app`.
4. Verify: trigger a webhook delivery (which writes a body
   snapshot) and confirm it lands in the bucket.
5. Once confirmed, revoke the old key in the Spaces dashboard.

Do not revoke the old key first; the running process will lose
access mid-flight.

## Webhook AEAD key

The webhook secret AEAD key (`webhook.aead_key`, env
`SHITHUB_WEBHOOK__AEAD_KEY`) encrypts every webhook's secret at
rest. It's separate from `auth.totp_key_b64` so rotating one
doesn't force rotating the other.

### First-time separation

If you're upgrading from a pre-separation deploy where webhook
secrets shared `auth.totp_key_b64`:

1. Generate a new key:
   `openssl rand -base64 32`.
2. Set `shithub_webhook_aead_key_b64` in `inventory/production`
   (or `SHITHUB_WEBHOOK__AEAD_KEY` directly on the box).
3. `make deploy ANSIBLE_INVENTORY=production` (rolls both web
   and worker; rendered env file now includes the new var).
4. Run `shithubd admin re-encrypt-webhooks --dry-run` and review
   the row count.
5. Run `shithubd admin re-encrypt-webhooks` for the real
   migration. Each row is re-encrypted from the legacy (TOTP)
   key onto the new dedicated key. Idempotent and resumable.
6. Verify by triggering a delivery — confirm signatures still
   validate at the receiver end.

The transition is safe to take in either order (deploy first vs
re-encrypt first) because `OpenSecret` tries the primary key
first and falls back to the legacy key for any row that hasn't
been migrated yet.

### Routine rotation (already separated)

1. Generate the new key:
   `openssl rand -base64 32`.
2. Swap `shithub_webhook_aead_key_b64` in inventory; the previous
   value should still be temporarily kept as a comment in case
   step 4 needs a rollback.
3. `make deploy ANSIBLE_INVENTORY=production`.
4. Run `shithubd admin re-encrypt-webhooks` to re-encrypt every
   row from the previous key onto the new one.

Failing to re-encrypt before fully retiring the previous key
disables every webhook (the auto-disable logic kicks in on
first decrypt failure). The new code's legacy fallback only
helps when `auth.totp_key_b64` is the previous key — for a
key-to-key rotation that didn't go through TOTP, the operator
must run re-encrypt before the previous key is purged.

## Operator SSH keys

Standard procedure: add the new key to every host's
`~operator/.ssh/authorized_keys`, log in with the new key to
confirm, remove the old. Ansible's `authorized_key` module makes
this idempotent; the `base` role will pick up changes if the
inventory's `operator_ssh_keys` list is the source of truth.

## Audit

Every rotation is logged in the host's journal (the deploy run's
output) and, for DB rotations, in `pg_stat_activity` history if
your retention allows. There's no centralized rotation log; if
you want one, capture each rotation in your team's incident
channel with date + class + reason.
