# Webhooks

The webhook **delivery format** (payloads, signing), the repository
webhook **management API**, and the organization-owner web UI for
organization webhooks are shipped. The REST management endpoints are
currently repo-scoped, PAT-authenticated, and
share the canonical [API conventions](overview.md) (JSON error
envelopes, `X-RateLimit-*`, `X-OAuth-Scopes`, `Link`
pagination).

## Delivery format

See [Webhooks (user docs)](../user/webhooks.md) for the full
delivery contract — headers, body framing, signature verification,
retries, idempotency.

The user-docs page is intentionally the canonical place; an API
consumer building a subscriber endpoint reads the same material.

## Management API

All endpoints require an `Authorization: Bearer <pat>` header
whose token carries the `repo:write` scope and the caller's role
on the repo must reach **settings:general** (owner / write
collaborator). Webhook secrets are write-only: they're set on
create and rotated by passing a new `secret` on PATCH, but
**never** returned in any response.

| Method | Path                                                                       |
|--------|----------------------------------------------------------------------------|
| GET    | `/api/v1/repos/{owner}/{repo}/hooks`                                       |
| POST   | `/api/v1/repos/{owner}/{repo}/hooks`                                       |
| GET    | `/api/v1/repos/{owner}/{repo}/hooks/{id}`                                  |
| PATCH  | `/api/v1/repos/{owner}/{repo}/hooks/{id}`                                  |
| DELETE | `/api/v1/repos/{owner}/{repo}/hooks/{id}`                                  |
| GET    | `/api/v1/repos/{owner}/{repo}/hooks/{id}/deliveries`                       |
| GET    | `/api/v1/repos/{owner}/{repo}/hooks/{id}/deliveries/{did}`                 |
| POST   | `/api/v1/repos/{owner}/{repo}/hooks/{id}/deliveries/{did}/redeliver`       |

### Create

```http
POST /api/v1/repos/alice/demo/hooks
Authorization: Bearer <pat>
Content-Type: application/json

{
  "url":             "https://hooks.example.com/sink",
  "content_type":    "json",
  "events":          ["push", "pull_request"],
  "active":          true,
  "ssl_verification": true,
  "secret":          "shared-secret-or-omit-to-mint"
}
```

`content_type` is `json` (default) or `form`. Omitting `secret`
mints a fresh one server-side. The server validates the URL
against the SSRF allow-list (scheme + port + non-private
resolution) so a 422 here means the target was rejected at
create time — no silent delivery failures later.

A successful create returns the hook row at `201 Created` and
enqueues a synthetic `ping` delivery so the operator sees an
immediate round-trip.

### Patch

```http
PATCH /api/v1/repos/alice/demo/hooks/42
Content-Type: application/json

{
  "url":    "https://hooks.example.com/v2",
  "events": ["push", "pull_request", "issues"],
  "active": false,
  "secret": "rotated-secret"
}
```

Fields are merged onto the current row: omit a field to keep its
existing value. Passing `secret` rotates the HMAC key used for
subsequent deliveries.

### Deliveries

The deliveries list is paginated (default 30, max 100 per page)
and emits standard `Link: <...>; rel="next" …` headers. The
single-delivery shape includes the captured request headers,
request body, and response body so operators can replay a
failed delivery from the recorded transcript.

`POST .../deliveries/{did}/redeliver` enqueues a fresh delivery
copying the same payload + headers under a new id; the response
body is `{"id": <new_delivery_id>}`.

## Event types (canonical list)

The events shippable today, by `X-Shithub-Event` header:

- `push`
- `pull_request` (actions: `opened`, `closed`, `merged`,
  `reopened`, `edited`, `ready_for_review`, `converted_to_draft`,
  `synchronize`)
- `pull_request_review` (actions: `submitted`, `dismissed`)
- `pull_request_review_comment`
- `issues` (actions: `opened`, `closed`, `reopened`, `edited`,
  `assigned`, `unassigned`, `labeled`, `unlabeled`)
- `issue_comment`
- `check_run` (actions: `created`, `completed`, `rerequested`)
- `check_suite` (actions: `requested`, `completed`,
  `rerequested`)
- `workflow_run` (actions: `queued`, `running`, `completed`)
- `workflow_job` (actions: `queued`, `running`, `completed`,
  `cancelled`)
- `star`
- `fork`
- `repository` (actions: `created`, `deleted`, `archived`,
  `unarchived`, `renamed`, `transferred`, `publicized`,
  `privatized`)
- `ping` (test event you trigger manually)

Each event's payload is documented per-type in the webhook detail
page's "Recent deliveries" inspector — that's currently the
authoritative reference until per-event documentation lands here.

### Actions payload safety

`workflow_run` and `workflow_job` payloads are structural snapshots:
ids, run index, workflow path/name, head SHA/ref, event kind, status,
conclusion, timestamps, job key/name, runner id, needs, timeout, and
cancellation state. They intentionally do **not** include workflow
event payloads, env, permissions, logs, runner JWTs, or secret values.
