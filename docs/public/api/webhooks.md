# Webhooks

The webhook **delivery format** (payloads, signing) is shipped
and stable. The webhook **management API** (CRUD over webhooks
on a repo) is planned.

## Delivery format

See [Webhooks (user docs)](../user/webhooks.md) for the full
delivery contract — headers, body framing, signature verification,
retries, idempotency.

The user-docs page is intentionally the canonical place; an API
consumer building a subscriber endpoint reads the same material.

## Management API (planned)

| Method | Path                                                      | Scope        |
|--------|-----------------------------------------------------------|--------------|
| GET    | `/api/v1/repos/{owner}/{repo}/hooks`                      | `webhooks`   |
| POST   | `/api/v1/repos/{owner}/{repo}/hooks`                      | `webhooks`   |
| GET    | `/api/v1/repos/{owner}/{repo}/hooks/{id}`                 | `webhooks`   |
| PATCH  | `/api/v1/repos/{owner}/{repo}/hooks/{id}`                 | `webhooks`   |
| DELETE | `/api/v1/repos/{owner}/{repo}/hooks/{id}`                 | `webhooks`   |
| POST   | `/api/v1/repos/{owner}/{repo}/hooks/{id}/pings`           | `webhooks`   |
| GET    | `/api/v1/repos/{owner}/{repo}/hooks/{id}/deliveries`      | `webhooks`   |
| POST   | `/api/v1/repos/{owner}/{repo}/hooks/{id}/deliveries/{delivery}/redeliver` | `webhooks` |

Until these land, manage webhooks via the web UI under
Repository → Settings → Webhooks.

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
