# Rotate signing keys

Distinct from `rotate-secrets.md` because signing keys have
external verifiers (subscribers, browsers, recipients) that need
to keep validating old artifacts during the rollover window.

## Webhook HMAC secrets

Per-webhook, not global. Each webhook has its own secret. The
owner rotates by editing the webhook in Settings → Webhooks →
the hook → "Rotate secret" → set new value.

- Subscriber MUST be updated to the new secret first (or accept
  both during the cutover).
- The previous secret is **not** kept; immediate rotation cuts
  off old-signature validation.
- For ops-driven mass rotation (suspected key store compromise):
  `shithubd webhook rotate-all` regenerates every secret. All
  subscribers will start getting `signature mismatch` until the
  owners update their endpoints.

## Notification unsubscribe HMAC

The one-click unsubscribe links in emails are HMAC-signed. The
key lives in `notif.unsubscribe_key`. Rotation invalidates every
**old email's** unsubscribe link (a recipient with an unread
email containing a link from before rotation will get "invalid
link" if they click it after rotation).

Procedure:

1. Generate a new key: `openssl rand -base64 32`.
2. Replace `notif.unsubscribe_key` in `worker.env`.
3. Redeploy worker (`ANSIBLE_TAGS=app`).
4. Optional: add a banner to the in-app inbox letting users
   know unsubscribe links from old emails will not work.

## Cursor signing key

`internal/pagination/keyset` HMACs every cursor we hand out.
Rotation invalidates every cursor in flight. The user-visible
effect is: any open browser tab with a "Next page" link from
before the rotation will return "invalid cursor" → take them
back to page 1.

Procedure mirrors the unsubscribe key. Acceptable to do without
warning; the worst case is a user clicks "Next" and gets sent
back to the top of the list.

## TLS certificates (Caddy-managed)

Caddy obtains and renews certs from Let's Encrypt automatically.
Manual rotation is rarely needed; if it is:

```sh
ssh edge
sudo caddy reload --config /etc/caddy/Caddyfile
```

If Let's Encrypt is rate-limiting you, switch the
`caddy_use_acme_staging` inventory flag to `true`, redeploy,
verify, then flip back. Production cert reissue is constrained
by LE's per-week limits; mass rotation in a single hour will
fail.

## SSH host keys

The host keys (`/etc/ssh/ssh_host_*`) are sshd's identity to
clients. Rotating them prompts every git client with "WARNING:
REMOTE HOST IDENTIFICATION HAS CHANGED!" — disruptive.

Only rotate if the host has been compromised. Procedure:

1. `ssh-keygen -A` on the host (regenerates all host keys).
2. `systemctl restart sshd`.
3. Publish the new host-key fingerprints somewhere users can
   verify (status page, security advisory).
4. Users prune the old line from `~/.ssh/known_hosts` and
   re-accept on first reconnect.

User-facing communication is critical here. Without it, you'll
look like a MITM attacker to every developer's SSH client.
