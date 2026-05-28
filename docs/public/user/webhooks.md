# Webhooks

Webhooks send HTTP POSTs to your URL when something happens in a
repo (push, PR opened, issue commented, etc.).

- Repository webhooks are configured at Repository → Settings →
  Webhooks → "Add webhook".
- Organization webhooks are configured at Organization settings →
  Integrations → Webhooks. They receive events for repositories owned
  by that organization.

## Configuration

- **Payload URL** — the HTTPS endpoint we POST to. HTTP is
  rejected on production instances.
- **Content type** — `application/json` (default) or
  `application/x-www-form-urlencoded`.
- **Secret** — used to HMAC-sign each delivery. We strongly
  recommend setting one. The secret is stored AEAD-encrypted at
  rest; you cannot retrieve it after creation, only replace it.
- **Events** — pick "Just push", "Send everything", or specific
  events.
- **Active** — toggle without deleting.

## Signature verification

Each delivery includes:

- `X-Shithub-Event: <event-name>` — e.g., `push`, `pull_request`,
  `workflow_run`.
- `X-Shithub-Delivery: <uuid>` — unique per delivery (idempotent).
- `X-Shithub-Signature-256: sha256=<hex>` — HMAC-SHA256 of the
  raw body using your configured secret.

**Always verify the signature before trusting the payload.**
Constant-time comparison; never compare with `==`.

### Go

```go
func verify(body []byte, sig, secret string) bool {
    sig = strings.TrimPrefix(sig, "sha256=")
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    expected := hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(sig), []byte(expected))
}
```

### Python

```python
import hmac, hashlib

def verify(body: bytes, sig: str, secret: str) -> bool:
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    sig = sig.removeprefix("sha256=")
    return hmac.compare_digest(sig, expected)
```

### Node.js

```js
const crypto = require("crypto");

function verify(body, sig, secret) {
  const expected = "sha256=" + crypto
    .createHmac("sha256", secret)
    .update(body)
    .digest("hex");
  return crypto.timingSafeEqual(
    Buffer.from(sig),
    Buffer.from(expected),
  );
}
```

The body must be the **raw request body**, not the parsed JSON.
Frameworks that auto-parse will give you the wrong bytes.

## Idempotency

Use `X-Shithub-Delivery` as your idempotency key. We may retry a
delivery if your endpoint returns 5xx or times out, so processing
the same delivery twice should be safe in your system.

## Retries

A delivery is retried on:

- Network error.
- 5xx response.
- Timeout (default 10s).

Retry schedule: exponential backoff with jitter, up to ~6 retries
over ~24h. After 50 consecutive failures, the webhook **auto-
disables** to stop bombarding a broken endpoint. You'll see a
banner on the webhook config page; flip "Active" back on once
the endpoint is fixed.

## Inspecting deliveries

Webhook detail page → "Recent deliveries". Each row shows:

- Event + delivery ID + timestamp.
- Request headers + (truncated) body we sent.
- Response status + headers + (truncated) body we got back.
- "Redeliver" — re-sends the original payload with the same
  signature.

Stored bodies are capped at 32 KiB (your endpoint can accept
bigger; we just don't keep more for the inspector).

## Actions events

Repository webhooks can subscribe to Actions lifecycle events:

- `workflow_run` actions: `queued`, `running`, `completed`.
- `workflow_job` actions: `queued`, `running`, `completed`,
  `cancelled`.

Actions payloads only carry structural run/job metadata. shithub does
not include workflow event payloads, env, permissions, logs, runner
tokens, or secrets in webhook bodies.

## SSRF defense

shithub validates webhook URLs server-side: hostnames are
resolved, IPs are checked against a block-list (RFC1918, link-
local, loopback, multicast, 169.254.0.0/16, etc.), and the
request is dialed to the resolved IP — no following CNAMEs into
internal address space at delivery time.

Operators of self-hosted instances can opt in to private
destinations via an `AllowedHosts` list — see
[self-host configuration](../self-host/configuration.md).
