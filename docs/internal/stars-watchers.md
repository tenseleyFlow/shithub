# Stars, watchers, and the events log (S26)

S26 ships three things that look small but underpin a lot of
downstream sprints: the `stars` table, the `watches` table, and the
canonical `domain_events` log that S29 (notifications) and S33
(webhooks) consume.

## Stars

`stars(user_id, repo_id, starred_at)` — PK `(user_id, repo_id)`.
Idempotent insert via `INSERT … ON CONFLICT DO NOTHING`. Two
`AFTER` triggers maintain `repos.star_count`:

* `stars_count_inc` — fires only on actual INSERT (the conflict
  path is silent), so re-starring an already-starred repo doesn't
  double-increment.
* `stars_count_dec` — `GREATEST(star_count - 1, 0)` defends
  against drift if the count ever underflows.

Read patterns:

* `ListStargazersForRepo(repo_id, limit, offset)` — paginated, sorted
  by `starred_at DESC`. Excludes suspended users (per the spec
  pitfall: "suspended users don't taint public lists").
* `ListStarsForUser(user_id, limit, offset)` — drives the `Stars`
  profile tab. Sorted by `starred_at DESC` per the spec's day-1 lean.
  Soft-deleted repos are filtered out.

## Watches

`watches(user_id, repo_id, level enum('all','participating','ignore'),
updated_at)` — PK `(user_id, repo_id)`. **Absence of a row is the
implicit `participating` default**, matching GitHub. The
`CurrentLevel` helper resolves missing rows to `participating` so
callers never need to special-case the absent state.

`repos.watcher_count` is the count of rows where `level <> 'ignore'`.
The `watches_count` trigger handles all four state-transition shapes
(insert non-ignore → +1; update across ignore boundary → ±1; delete
non-ignore → −1). Test coverage in
`internal/social/social_test.go::TestSetWatch_TransitionsAcrossIgnore`.

### Auto-watch

Two non-destructive entry points populate `watches` rows when none
exists:

* `AutoWatchOnCollab(userID, repoID)` — invoked by S15's
  collaborator-add path. Inserts `level='all'` so collabs get full
  notifications by default.
* `AutoWatchOnInvolvement(userID, repoID)` — invoked by issue
  create, issue comment, PR create, mention resolution, and review
  request. Inserts `level='participating'`.

Both use `INSERT … ON CONFLICT DO NOTHING`, so a user who has
explicitly chosen `ignore` (or any other level) keeps their choice.
This is the spec's "permission revocation cascade" rule applied at
write time: we never overwrite a user's preference, regardless of
the trigger source.

The collaborator-add wiring lands when S32 ships the collaborators
UI; for now `AutoWatchOnCollab` is callable but unwired.

## domain_events

`domain_events(id, actor_user_id, kind, repo_id NULL, source_kind,
source_id, public bool, payload jsonb, created_at)` is the canonical
event log for both notifications fan-out (S29) and webhook delivery
(S33). The S26 spec called for an `events` table; the S00-S25 audit
flagged that S29 and S33 would each want their own log if we landed
that as a parallel structure. We adopted the unified shape from day
1 to avoid migrating twice.

S26 emits two event kinds today:

* `kind = "star"` (`source_kind = "repo"`, `source_id = repo_id`,
  `public = repoIsPublic`)
* `kind = "unstar"` (same shape)

Future sprints add their own kinds (`issue_comment_created`,
`pr_opened`, `check_failed`, …). The `kind` and `source_kind`
columns are intentionally `text` rather than enums: kinds emerge
incrementally; we don't want a migration per kind.

### Public-flag policy

* Public-repo events → `public = true`. S42's Home and Explore feeds
  surface these in authenticated and public timelines.
* Private-repo events → `public = false`. Visible only to repo
  collaborators via per-row visibility checks at read time.
* User-scoped events (e.g. follow events) → public only when the action
  is safe to expose on the public timeline.

The handler/orchestrator is the source of truth for the public flag
— `social.Star` reads `repo.visibility` and passes the bool through.
**Always re-check visibility at fan-out time** (S29's spec
explicitly calls this out): a repo can flip from public to private
between event-emit and fan-out, and a stale-read public event must
not leak into a public feed after the flip.

## Permission lattice

Two new policy actions:

* `star:create` — login-required only (any logged-in user with read
  access). Suspended actors blocked at the `Can` step before this.
* `watch:set` — same shape: login-required, no role gate.

Both rely on the visibility short-circuit at step 4 of `Can` to
deny anonymous access to private-repo stars/watches with the
404-leak-safe code.

## Rate limit

100 star/unstar actions per hour per user, scoped via the
`throttle.Limit{Scope: "star", Identifier: "user:N"}` envelope
shared with comment / PAT / repo-create rate caps. S35 owns the
broader abuse heuristics; this is the floor against trending
manipulation.

## Forward links

* **S29 (notifications):** consume `domain_events` for fan-out.
  The `watches` rows route per-repo events; the `notification_threads`
  table (introduced in S29) routes per-thread subscriptions.
* **S33 (webhooks):** consume `domain_events` for delivery. Same
  table, different consumer; the `id` column is the cursor for
  both.
* **S32 (settings UI):** wire `AutoWatchOnCollab` into the
  collaborator-add path.
* **S42 (social feed):** read public events for Home/Explore and cached
  trending rankings.

## What S11 promised but didn't ship

The S11 status block claimed `repos.star_count` and
`repos.watcher_count` were "already declared". They were not — the
0017_repos.sql migration doesn't include them. S26 adds them in
0026_stars.sql. Noted here so future archaeology doesn't blame
the migration for "missing" columns referenced in the spec.
