# Notifications (S29)

S29 ships in-app notifications + email. The fan-out worker consumes
from `domain_events` (the unified S26 event log) and materializes per-
recipient inbox rows + (optionally) sends one email. Per-thread
coalescing keeps the inbox slim; a per-recipient + per-thread storm
dampener keeps inboxes sane during bot-spam scenarios.

## Architecture

```
internal/notif/
    notif.go         — Deps, FanoutBatch, storm-dampener constants
    routing.go       — Reason / Relation / Action; Routing(kind, rel)
    fanout.go        — FanoutOnce(ctx, deps): drain N events from cursor
    email.go         — sendNotificationEmail + HMAC-signed unsub URL
    emit.go          — Emit / EmitTx: insert one domain_events row
    mentions.go      — ResolveMentionUserIDs (markdown → user IDs)
    queries/         — sqlc input
    sqlc/            — generated DB code

internal/worker/jobs/notify_fanout.go   — wraps FanoutOnce as a worker job
internal/web/handlers/notifications/    — /notifications + thread sub
internal/web/templates/notifications/   — inbox.html, unsubscribed.html
```

## Migrations

* **0033 — notifications**:
  * `notification_thread_kind` enum (`'issue' | 'pr'`).
  * `notifications` (per-recipient inbox row, with thread-coalesce
    unique partial index on `(recipient_user_id, thread_kind, thread_id)`
    where `thread_id IS NOT NULL`).
  * `notification_threads` (per-recipient, per-thread subscription
    override — the row that the Subscribe / Unsubscribe / Ignore
    handlers write).
  * `notification_email_log` (de-dup + storm dampener input).
  * `domain_events_processed` (cursor table; one row per consumer).
    Seeded with `('notify_fanout', 0)` so the worker's first run
    starts at the head.

## Event flow

```
caller (issues/pulls/etc) ──▶ notif.Emit(...)
                                │
                                ▼
                          domain_events  (one row, NOTIFY shithub_jobs)
                                │
                                ▼
                       notify:fanout job
                                │
                                ▼
                      FanoutOnce(ctx, deps)
                                │
                                ├── drain N events from cursor
                                ├── per event, computeRecipients()
                                ├── visibility re-check per recipient
                                ├── self-suppress
                                ├── pickAction() per recipient
                                ├── upsertInboxRow (coalesce by thread)
                                └── maybeSendEmail (storm-dampened)
```

## Routing matrix

`Routing(kind, relation)` is the single source of truth for "who
gets pinged on what." Today it's a switch; once we have ~30 kinds
it'll graduate to a table-driven shape.

| Event kind                            | Relation        | Inbox | Email | Override-ignore | Reason            |
|--------------------------------------|-----------------|:----:|:-----:|:---------------:|-------------------|
| `issue_created`, `pr_opened`         | mention         | ✓    | ✓     | **yes**         | mention           |
| `issue_created`, `pr_opened`         | watching=all    | ✓    | ✓     | no              | watching          |
| `issue_comment_created`, `pr_comment_created` | mention | ✓ | ✓ | **yes**         | mention           |
| `issue_comment_created`, `pr_comment_created` | assignee | ✓ | ✓ | no              | assignment        |
| `issue_comment_created`, `pr_comment_created` | author   | ✓ | ✓ | no              | author            |
| `issue_comment_created`, `pr_comment_created` | commenter| ✓ | ✓ | no              | commenter         |
| `issue_comment_created`, `pr_comment_created` | subscribed-thread | ✓ | ✓ | no       | subscribed        |
| `issue_comment_created`, `pr_comment_created` | watching=all      | ✓ | ✓ | no       | watching          |
| `issue_assigned`, `pr_assigned`      | assignee        | ✓    | ✓     | no              | assignment        |
| `issue_closed`, `issue_reopened`, `pr_closed`, `pr_reopened`, `pr_merged` | author / assignee / sub / watch | ✓ | ✓/no | no | author/assignment/subscribed/watching |
| `review_requested`                   | reviewer        | ✓    | ✓     | **yes**         | review_requested  |
| `review_submitted`                   | author / sub    | ✓    | ✓/no  | no              | author/subscribed |
| `mentioned` (standalone)             | mention         | ✓    | ✓     | **yes**         | mention           |
| `check_failed`, `check_fixed`        | author          | ✓    | no    | no              | author            |
| `repo_archived` and S16 lifecycle    | repo_owner      | ✓    | ✓     | no              | repo_admin_action |

Anything else → `Skip` (deny by default).

## Storm dampener

* Per (recipient, thread, time-window): max **1 email per 10 minutes**
  per thread.
* Per recipient absolute: max **100 emails per hour**.
* In-app inbox rows are **not** damped (they're cheap and the
  thread-coalesce already collapses noise).

## Auto-subscription rules

When a recipient earns a slot via author / assignee / reviewer /
mention / commenter, the fan-out worker writes a
`notification_threads` row with `subscribed=true` if no row exists.
Explicit user choices (a manual Subscribe / Unsubscribe) win
because the auto-sub uses `INSERT … ON CONFLICT DO NOTHING`.

## Visibility re-check

The fan-out worker re-checks repo visibility *per recipient* before
materializing the inbox row. This catches the case where a repo
flipped from public to private between the event-emit moment and the
fan-out moment — a stale-read public event must not produce a
notification for a recipient who no longer has access.

The check uses `policy.IsVisibleTo` (same predicate the rest of the
runtime uses for "can this viewer see this repo?").

## One-click unsubscribe

The email body carries an HMAC-signed URL of the shape:

```
{baseURL}/notifications/unsubscribe?u={recipientID}&tk={kind}&ti={threadID}&sig={base64(HMAC-SHA256)}
```

The handler verifies the HMAC and, on success, writes a
`notification_threads` row with `subscribed=false,
reason='email_one_click'`. No session is required — RFC 8058 mailers
click these from arbitrary agents.

The HMAC key (`Notif.UnsubscribeKeyB64`) is operator-managed; rotating
it invalidates outstanding unsub links. The dev-mode wiring derives a
deterministic key from the session secret and logs a WARN — operators
must set the explicit key in prod.

## Routes

| Method | Path                                    | Auth     | Notes                              |
|-------|-----------------------------------------|----------|------------------------------------|
| GET   | `/notifications`                        | required | Inbox (filter=unread tab)          |
| POST  | `/notifications/{id}/read`              | required | Mark one read                      |
| POST  | `/notifications/{id}/unread`            | required | Mark one unread                    |
| POST  | `/notifications/mark-all-read`          | required | Bounded sweep                      |
| POST  | `/threads/{kind}/{id}/subscribe`        | required | Per-thread subscribe override      |
| POST  | `/threads/{kind}/{id}/unsubscribe`      | required | Per-thread unsubscribe override    |
| GET   | `/notifications/unsubscribe`            | none     | One-click HMAC-signed unsub        |

## What we deferred from the spec

* **API endpoint** `GET /api/v1/notifications` + the per-thread sub
  PUTs. Defer to S33 / S34 API consolidation (matches the S28
  search-API deferral).
* **Per-kind HTML email templates**. Today every email uses a
  minimal HTML+plaintext layout that includes the reason, event
  kind, and link. Per-kind templates ride a follow-up that touches
  the email surface. The List-Unsubscribe URL is still wired in
  the body footer.
* **List-Unsubscribe HTTP header** (RFC 8058). The
  `email.Message` shape from S05 doesn't carry arbitrary headers;
  adding `Headers map[string]string` ripples through SMTP +
  Postmark backends. Tracked as a follow-up; the URL is still
  emitted in the body so the one-click flow works.
* **PR review submission, PR merge, check-failed/fixed event
  emission**. The routing matrix is wired for these kinds; the
  emit-side hooks land when the spec touches the PR review and
  check pipelines next.
* **S16 lifecycle email kinds emission** (`repo_archived` etc).
  Routing is wired; the emit-side hook lands when the lifecycle
  ops next get touched.
* **Daily digest mode**. Schema accommodates (one inbox row per
  thread); the cron + render path is post-MVP.
* **Reply-by-email** is post-MVP per the spec.

## Pitfalls noted in code

* **Visibility leak**: highest-stakes risk; addressed by the
  per-recipient re-check at fan-out time. Test:
  `TestFanout_VisibilityRecheck_PrivateRepo`.
* **Email storm**: dampener caps. Test:
  `TestFanout_StormDampener_SingleEmailPerThread`.
* **Thread coalescing**: 5 events → 1 inbox row. Test:
  `TestFanout_ThreadCoalescing`.
* **Self-notification suppression**: dropped at recipient-compute
  time AND defense-in-depth at dispatch. Test:
  `TestFanout_SelfSuppression`.
* **Override-ignore for mentions**: routing flag honored over the
  `watches.level=ignore` gate. Test:
  `TestFanout_MentionOverridesIgnore`.
* **Unsubscribe HMAC tampering**: signature mismatch invalidates.
  Test: `TestUnsubscribe_HMACRoundtrip`.
* **Suspended user as actor**: drop at recipient-compute when the
  primary email is unverified or the user is soft-deleted. Mention
  resolver also filters suspended.
* **Event ordering**: fan-out drains in `id ASC` order;
  `domain_events.id` is bigserial so commit order is preserved.
