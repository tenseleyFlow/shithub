# Issues, comments, labels, milestones

S21 ships the issues subsystem: issues + threaded comments, labels,
milestones, the polymorphic timeline event log, and the cross-reference
index. The same tables are reused by S22 for pull requests via the
`issue_kind` discriminator.

## Schema (migration 0022)

```
repo_issue_counter   — per-repo monotonic numbering (issues + PRs share)
issues               — the row; kind discriminator covers PR rows in S22
issue_comments       — per-issue threaded comments
issue_assignees      — many-to-many issue ↔ user
labels               — repo-scoped labels
issue_labels         — many-to-many issue ↔ label
milestones           — repo-scoped milestones
issue_events         — generic timeline events (closed, locked, labeled, …)
issue_references     — cross-reference index (comment / issue / commit → issue)
```

The `kind` column on `issues` is in from day one (`'issue' | 'pr'`) so
S22's PR rows are first-class without a schema split. PR-specific
fields land in a `pull_requests` table keyed on `issues.id`.

### Per-repo numbering

`repo_issue_counter (repo_id, next_number)` is allocated lazily at first
issue creation and on every repo create (S16's `repos.Create` calls
`EnsureRepoIssueCounter` inside the create tx). Allocation is one
`UPDATE … RETURNING` inside the same tx as the issue insert:

```sql
UPDATE repo_issue_counter
SET next_number = next_number + 1
WHERE repo_id = $1
RETURNING (next_number - 1)::bigint AS allocated;
```

Postgres serializes concurrent updaters on the row lock, so concurrent
`issues.Create` calls on the same repo end up with distinct numbers.
The race test (`TestCreate_ConcurrentRaceForUniqueNumbers`) fans out 8
goroutines and asserts no duplicates.

### Default labels

Repo create seeds the GitHub-aligned set:

```
bug · d73a4a · Something isn't working
documentation · 0075ca · Improvements or additions to documentation
duplicate · cfd3d7 · This issue or pull request already exists
enhancement · a2eeef · New feature or request
good first issue · 7057ff · Good for newcomers
help wanted · 008672 · Extra attention is needed
invalid · e4e669 · This doesn't seem right
question · d876e3 · Further information is requested
wontfix · ffffff · This will not be worked on
```

Seeding runs inside the repo-create transaction via
`issues.SeedDefaultLabels(ctx, tx, repoID)`. It swallows unique-violations
so re-runs are no-ops; backfill for repos created before S21 is a one-off
SQL `INSERT … ON CONFLICT DO NOTHING`.

## Routes

| Route                                                          | Auth          |
| -------------------------------------------------------------- | ------------- |
| `GET  /{owner}/{repo}/issues?state=open\|closed`               | public        |
| `GET  /{owner}/{repo}/issues/{number}`                         | public        |
| `GET  /{owner}/{repo}/labels`                                  | public        |
| `GET  /{owner}/{repo}/milestones`                              | public        |
| `GET  /{owner}/{repo}/issues/new`                              | RequireUser   |
| `POST /{owner}/{repo}/issues`                                  | RequireUser   |
| `POST /{owner}/{repo}/issues/{number}/comments`                | RequireUser   |
| `POST /{owner}/{repo}/issues/{number}/state`                   | RequireUser   |
| `POST /{owner}/{repo}/issues/{number}/lock`                    | RequireUser   |
| `POST /{owner}/{repo}/issues/{number}/labels`                  | RequireUser   |
| `POST /{owner}/{repo}/issues/{number}/milestone`               | RequireUser   |
| `POST /{owner}/{repo}/issues/{number}/assignees`               | RequireUser   |
| `POST /{owner}/{repo}/labels` + `/{id}/update` + `/{id}/delete` | RequireUser  |
| `POST /{owner}/{repo}/milestones` + `/{id}/update`             | RequireUser   |
| `POST /{owner}/{repo}/milestones/{id}/state` + `/{id}/delete`  | RequireUser   |

Public reads still pass through `policy.Can(ActionIssueRead)` so
private repos return the existence-leak 404. RequireUser on writes
gives anonymous browsers a `/login` redirect instead of the same 404.

## Cross-reference indexing

`internal/issues/references.go::insertReferencesFromBody` parses
`#N` and `owner/repo#N` from the comment / issue body inside the
creating transaction. Each parsed reference produces:

1. an `issue_references` row pointing source → target,
2. a `referenced` event emitted on the *target* issue's timeline.

The cross-repo regex runs first; matched ranges are stripped before
the same-repo regex sweeps so `alice/proj#3` doesn't also trip the
`#3` matcher. Self-references skip silently. Unknown owners / repos /
issue numbers are best-effort dropped — malformed references shouldn't
fail the create.

Commit-message references (S14's `push:process`) call into the same
helper with `srcKind = "commit_message"` and `srcObjectID = push_event_id`
once that worker job ships.

## Markdown + sanitization

Issue bodies and comments use the existing S17 markdown pipeline
(`internal/repos/markdown.RenderHTML`): Goldmark for parsing,
bluemonday's `UGCPolicy` for sanitization. Rendered HTML is cached on
`body_html_cached`; `md_pipeline_version` lets the worker re-render on
pipeline upgrades (post-MVP).

`<script>` tags are stripped — the sanitization test
(`TestCreate_RendersHTMLAndSanitizesScripts`) injects one and asserts
the cached HTML doesn't contain `<script` while still rendering
`**bold**` as `<strong>bold</strong>`.

## Locked issues

`SetLock` flips `locked` + emits a `locked`/`unlocked` event. Comments
hit the locked gate inside `AddComment`: non-collaborators get
`ErrIssueLocked`. The `IsCollab` flag is the caller's responsibility —
for v1 the web handler treats the repo owner as the only collaborator;
S15's `repo_collaborators` table is wired but the lookup is deferred.

## Rate limit

Comment creation hits the existing throttle bucket
(`scope=issue_comment`, `identifier=user:N`) at **20 per hour**.
Issue creation isn't independently rate-limited; the broader
`repo_create` bucket and account anti-abuse measures cover the
spam-floor case for now.

## Timeline events

`issue_events` is intentionally polymorphic: `kind` is a free string,
`meta` is a `jsonb` blob, and `ref_target_id` is a denormalized
pointer for events that target another issue (`referenced`,
`marked_as_duplicate_of`, etc.). Known kinds today:

```
closed, reopened, locked, unlocked,
labeled, unlabeled,
assigned, unassigned,
milestoned, demilestoned,
referenced
```

`meta` always carries an empty `'{}'::jsonb` when no metadata is
applicable — Go's `nil` byte slice would bind as SQL NULL and trip
the NOT NULL constraint, so the orchestrator passes `[]byte("{}")`
explicitly.

## Tests

| File                                              | What it covers                         |
| ------------------------------------------------- | -------------------------------------- |
| `internal/issues/references_test.go`              | regex behaviour for #N and owner/repo#N |
| `internal/issues/issues_test.go::Sequential…`     | per-repo number allocation             |
| `internal/issues/issues_test.go::ConcurrentRace…` | 8-goroutine fan-out, no duplicates     |
| `internal/issues/issues_test.go::Sanitizes…`      | XSS + markdown render                  |
| `internal/issues/issues_test.go::LockedRejects…`  | locked-issue gate for non-collab       |
| `internal/issues/issues_test.go::EmitsEvent`      | close → reopen event ordering          |
| `internal/issues/issues_test.go::CrossRef…`       | #N in body emits `referenced` on target |

## Deferred

- **Mention notifications** — `extractMentions` is in place, but the
  notifications surface (S29-area) hasn't shipped, so parsed `@username`
  tokens currently don't emit anything.
- **Commit-message references** — the helper accepts
  `srcKind = "commit_message"` already; wiring lives with S14's worker
  job (push:process).
- **Cross-org references** — the cross-repo regex resolves only
  user-owned repos; org owners arrive in S31.
- **Required signing / status checks on comments** — out of scope.
- **Reactions, edit history, comment edits across users** — post-MVP UI
  polish (S30+).
- **Server-side label / assignee / milestone filtering** — current
  list query filters by state + kind only; richer filtering is the
  perf-pass item in S36.
- **Issue templates / forms** — post-MVP.
- **Reactions** — post-MVP (own table; doesn't touch the schema here).
