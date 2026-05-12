# Issue events / timeline

Read-only access to an issue's recorded timeline events: every
state mutation (labeled, unlabeled, milestoned, demilestoned,
locked, unlocked, closed, reopened, referenced, …) inserts a row
in `issue_events` and surfaces here.

Scope: `repo:read`.

## Endpoint

```
GET /api/v1/repos/{owner}/{repo}/issues/{number}/events
```

Paginated with the standard `Link:` headers (default per_page 30,
max 100). Events are sorted **oldest-first** to match gh and the
HTML issue view.

## Response shape

```json
[
  {
    "id":              17,
    "kind":            "labeled",
    "actor_user_id":   42,
    "actor_username":  "alice",
    "meta": {
      "label_id":   8,
      "label_name": "bug"
    },
    "ref_target_id":   null,
    "created_at":      "2026-05-12T18:00:00Z"
  }
]
```

- `kind` is the event discriminator. Known values today include
  `closed`, `reopened`, `locked`, `unlocked`, `labeled`,
  `unlabeled`, `milestoned`, `demilestoned`, `referenced`,
  `merged`, and `head_ref_force_pushed` (the last two on PR
  threads). New event kinds are added over time; clients should
  treat unknown kinds as opaque.
- `actor_user_id` / `actor_username` are omitted when the event
  was system-emitted (no actor) or when the actor row was hard-
  deleted. The timeline is historical truth — suspending or
  deleting a user does not retroactively erase their authored
  events.
- `meta` is the event's structured payload, returned verbatim as
  it was recorded. The keys vary by `kind`; e.g. `labeled`/
  `unlabeled` carry `label_id` + `label_name`, `closed` may carry
  `comment_id` when the close was paired with a comment.
- `ref_target_id` is populated for `referenced` events (the
  cross-referenced issue's id) and otherwise omitted.

## Errors

- `404` — repo or issue doesn't exist (uniform envelope; can't
  enumerate via response shape).
- `403` — PAT lacks `repo:read`.

## Related

The PR-side equivalent (review submissions, push events, merge
events) is recorded into the same `issue_events` table because PR
records share the underlying `issues` row; both surfaces are
served by this endpoint.

For higher-level activity feeds, see [Events / activity](./events.md)
(repo-scoped and user-scoped domain events).
