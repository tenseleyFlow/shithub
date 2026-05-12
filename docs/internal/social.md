# Social Feed

S42 turns the S26 social primitives into a GitHub-like network surface:
follow graph, signed-in Explore/Home feed, public Explore feed, and cached
trending rankings.

## Follow Graph

`follows` stores one follower user and exactly one target:

- `followee_user_id` for user profiles.
- `followee_org_id` for organization profiles.

The schema enforces target XOR, blocks user self-follows, cascades on
deleted users/orgs, and uses partial unique indexes so follow/unfollow is
idempotent. State changes go through `internal/social` and record audit
rows when an audit recorder is supplied.

Follow actions emit public user-scoped `domain_events`:

- `followed_user`, `source_kind = "user"`, `source_id = target_user_id`
- `followed_org`, `source_kind = "org"`, `source_id = target_org_id`

The web layer exposes profile/org Follow buttons and follower/following
tabs. Suspended actors are rejected by middleware/policy before mutation.

## Feeds

Feeds read from `domain_events`; handlers never hand-roll visibility
logic. Public feeds require `domain_events.public = true`, non-deleted
actors, non-suspended actors, and a current public repo if the event is
repo-scoped. This second repo visibility check is load-bearing: an event
emitted while a repo was public must not leak after the repo becomes
private.

After sign-in, the default destination is `/explore`. `/` remains the
public build/landing page so the top-left shithub brand is always a way
back to the instance/version stamp.

The signed-in `/explore` feed includes:

- the viewer's own public activity,
- public activity from followed users,
- public activity from repos the viewer watches,
- public activity from repos owned by followed orgs,
- public org-scoped activity for followed orgs.

Anonymous Explore uses the global public feed. Both feeds page with a
keyset cursor over `(created_at, id)`. Full-page requests with a
`before` cursor still render that cursor window for no-JavaScript
fallbacks; HTMX requests return only the next feed rows plus a replacement
`More` control, so the browser appends activity in place like GitHub's
dashboard feed.

## Event Kinds

Current feed sources include:

- `repo_created`
- `push`
- `star` / `unstar`
- `forked`
- `issue_created`, comments, close/reopen, assignment events
- `pr_opened` and pull-request comment events
- `followed_user` / `followed_org`

`unstar` events remain in `domain_events` for audit/product history, but
the feed queries suppress them because GitHub does not surface unstars as
activity feed stories.

The `kind` and `source_kind` columns remain text. New product surfaces
can add events without a schema migration as long as their payload is
small JSON and the public flag is set conservatively.

## Trending

`trending_snapshots` stores denormalized rankings for day/week/month
windows and two kinds:

- `repos`
- `users`

The `trending:compute` worker job captures all six snapshots. A job with
an empty payload schedules its next run one hour later; pass
`{"schedule_next":false}` for a one-off recompute. Explore reads the
latest weekly snapshot and falls back to live computation before the
first worker run.

The repo score weights recent public stars, forks, and unique push
actors. The user score weights recent followers plus recent public event
activity.

## Operational Notes

Seed the recurring job once after deploy:

```sql
INSERT INTO jobs (kind, payload) VALUES ('trending:compute', '{}');
SELECT pg_notify('shithub_jobs', '');
```

The job is safe to re-run. Multiple recurring seeds produce multiple
hourly refresh jobs, so operators should keep one scheduled chain per
instance unless they intentionally want a shorter effective interval.
