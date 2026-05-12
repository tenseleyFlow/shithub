# Events / activity feed

Read-only activity feed over the `domain_events` table. Mirrors
GitHub's `/repos/{o}/{r}/events` and `/users/{username}/events`
endpoints.

## Endpoints

```
GET /api/v1/repos/{owner}/{repo}/events[?page=&per_page=]
GET /api/v1/users/{username}/events[?page=&per_page=]
```

Both are paginated; `Link:` headers emit `next`/`prev` only
(no `last`) since `domain_events` is append-only and high-churn
— we don't compute a cheap total count.

## Auth & visibility

| Endpoint | Scope | Visibility |
|----------|-------|------------|
| repo feed | `repo:read` (gates on `ActionRepoRead`) | All events for the repo, including non-public rows |
| user feed | `user:read` | **Only** events flagged `public=true` |

The user feed is intentionally narrow — it matches gh, which
never surfaces private-repo activity on a public user feed. A
private push by alice into a private repo will not appear on
`/users/alice/events` even when alice herself is the caller.

## Event shape

```json
{
  "id":       7142,
  "kind":     "pushed",
  "public":   true,
  "actor_id": 42,
  "repo_id":  17,
  "source":   { "kind": "repo", "id": 17 },
  "payload":  { "ref": "refs/heads/trunk", "commits": 3 },
  "created_at": "2026-05-12T18:00:00Z"
}
```

- `kind` is the canonical event name (`pushed`, `forked`,
  `starred`, `repo_created`, `issued`, `commented`, …). The
  vocabulary is open-ended; clients should pass through unknown
  kinds rather than 4xx.
- `payload` is opaque JSON whose shape varies by kind. Treat it
  as a free-form object; don't depend on specific keys unless
  documented elsewhere.
- `source.{kind,id}` ties the event back to the entity it was
  fired from (most commonly the repo itself; for issue/PR-level
  events, the issue or pull row id).

## Errors

- `404` — repo not visible / user doesn't exist.
- `403` — PAT lacks the required scope.
