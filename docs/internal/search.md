# Search (S28)

S28 ships a working search experience for repos, issues/PRs, users,
and code (paths + small-file content). Backed entirely by Postgres
FTS (`tsvector`) plus `pg_trgm` for code-identifier substring matches.
Visibility scoping flows through one composer
(`policy.VisibilityPredicate`) so every search query is gated by the
same rule the rest of the runtime uses.

## Architecture

```
internal/search/
    search.go        — Deps + Result types + page-size constants
    query_parse.go   — GitHub-style operator parser
    repos.go         — SearchRepos
    issues.go        — SearchIssues
    users.go         — SearchUsers
    code.go          — SearchCode (paths + content union)

internal/auth/policy/
    visibility_predicate.go  — composes the WHERE-clause fragment

internal/web/handlers/search/
    search.go        — /search and /search/quick handlers

internal/web/handlers/repo/
    search_query.go  — repo-local issue/PR query scoping helpers

internal/worker/jobs/
    repo_index_code.go        — re-indexes a repo's default branch
    repo_index_reconcile.go   — heals drift between
                                default_branch_oid and
                                last_indexed_oid
```

## Migrations

* **0030 — extensions**: `pg_trgm`, `unaccent`. Both ship with
  PostgreSQL contrib.
* **0031 — search indexes**: `repos_search`, `issues_search`,
  `users_search` tables, each keyed 1:1 with the source row and
  maintained by AFTER triggers. A custom text-search config
  `shithub_search` chains `unaccent` + `english_stem` so "café"
  matches "cafe" on both index and query sides. Backfill runs at
  migration time so existing rows are immediately searchable.
* **0032 — code search**: `code_search_paths` (paths only — cheap
  to populate; size cap doesn't apply) and `code_search_content`
  (paths + content for files ≤ 256 KiB AND text). Both have GIN
  indexes on `tsv`; `code_search_content` additionally has a GIN
  trigram index on the raw content for camelCase / snake_case
  substring matches the FTS tokenizer mangles. `repos.last_indexed_oid`
  added so the reconciler can detect drift.
* **0041 — repo owner terms**: repo search documents include the
  owning user/org handle plus display name, and result queries
  resolve owners through either `users` or `orgs`. This keeps public
  org repositories searchable by both `owner/repo` and owner-only
  text queries.

## Visibility predicate

`policy.VisibilityPredicate(actor, alias, startPlaceholder)`
returns a SQL fragment + bind args that filters a `repos` table
reference to rows visible to the actor. Rules:

* Soft-deleted repos: always excluded.
* Site admin: only the soft-delete filter applies.
* Anonymous: `visibility = 'public'`.
* Logged-in: public OR owner OR collaborator (any role).

The composer is the **single source of truth** for "what repos can
this viewer see in a list query." The S28 search functions thread it
through every per-type query; future listing endpoints (trending,
activity feed) reuse it.

The boundary is exercised by the search test suite at
`internal/search/search_test.go`:

* `TestSearchRepos_AnonymousSeesOnlyPublic` — anon never sees
  private rows for any free-text query.
* `TestSearchRepos_NonCollabOnPrivate` — non-collab logged-in
  user gets zero hits on a private-repo's content.
* `TestSearchRepos_OwnerSeesPrivate` — owner branch.
* `TestSearchRepos_CollabSeesPrivate` — collab-row branch.
* `TestSearchIssues_AnonymousSeesOnlyPublic` — issue-side mirror
  of the repo test (issues inherit visibility from their repo).

## Query parsing

`search.ParseQuery(raw)` splits the user query into:

* `Text` — free-text portion (drives `plainto_tsquery`).
* `Phrase` — when a quoted span is present (drives `phraseto_tsquery`).
* `Terms` / `ExcludedTerms` — positive and negated text terms. Negated
  terms are retained for callers that can enforce them; FTS callers do not
  broaden the query by treating them as positives.
* `RepoFilter` — `repo:owner/name` becomes `{Owner, Name}`.
* `StateFilter` — `is:open` / `is:closed` / `state:open` /
  `state:closed`. Aliases.
* `KindFilter` — `is:issue` / `is:pr` / `type:issue` / `type:pr` for
  issue-vs-PR scoping.
* `MergedStateFilter` — `is:merged` / `is:unmerged`; these imply the PR
  surface and conflict with an issue-only scope.
* `LockedFilter` — `is:locked` / `is:unlocked`.
* `AuthorFilter`, `AssigneeFilter`, `CommenterFilter` — username-backed
  issue/PR filters.
* `AssigneeAnyFilter` — `assignee:*`.
* `MentionFilter`, `InvolvesFilters` — participant filters. The execution
  layer resolves `@me` from the current actor so anonymous `@me` searches
  safely produce no rows.
* `ReviewRequestedFilter` — `review-requested:handle` / `review-requested:@me`
  for active, unsatisfied PR review requests.
* `MissingFilters` — `no:label`, `no:milestone`, `no:assignee`,
  `no:project`.
* `SortFilter` — normalized GitHub-style sort directives. Supported values are
  `sort:updated`, `sort:created`, `sort:comments`, `sort:relevance`, and their
  `-asc` / `-desc` variants. Bare values default to descending order.
* `OwnerFilter` — `user:handle` and `org:handle` for owner scoping.
* `LabelFilters`, `MilestoneFilter` — repeated labels are ANDed.
* `LanguageFilter`, `VisibilityFilter`, `ForkFilter`, `ArchivedFilter`,
  `TopicFilters` — repository metadata filters.
* `PathFilter`, `ExtensionFilter` — code path filters.
* `CreatedFilter`, `UpdatedFilter`, `ClosedFilter`, `MergedFilter` —
  exact dates, ranges, and comparison operators.

Unknown operator-shape tokens still fall through as free text. This keeps
future operator additions backwards-compatible and lets users naturally type
":"-containing strings without surprises.

The parser caps input at `MaxQueryBytes` (256) to defend against
pathological-length queries; longer inputs are silently truncated.

## GitHub-style qualifiers

Global result pages currently execute these recognized qualifiers:

* repositories: `repo:`, `user:`, `org:`, `language:`, `is:public`,
  `is:private`, `is:fork`, `is:archived`, `visibility:`, `fork:`,
  `archived:`, `topic:`, `created:`, `updated:`.
* issues and pull requests: `repo:`, `user:`, `org:`, `is:issue`,
  `is:pr`, `type:issue`, `type:pr`, `is:open`, `is:closed`, `state:`,
  `is:merged`, `is:unmerged`, `is:locked`, `is:unlocked`, `author:`,
  `assignee:`, `assignee:*`, `commenter:`, `mentions:`, `involves:`,
  `review-requested:`,
  `label:`, `milestone:`, `no:label`, `no:milestone`, `no:assignee`,
  `no:project`, repository metadata filters, `created:`, `updated:`,
  `closed:`, `merged:`, `sort:updated`, `sort:created`, `sort:comments`,
  `sort:relevance`.
* code: `repo:`, `user:`, `org:`, `language:`, repository metadata
  filters, `path:`, `extension:`, plus free text against indexed paths
  and small-file content.

Repo-local `/issues` and `/pulls` pages reuse `SearchIssues` instead of
maintaining a second SQL dialect. The web helper injects the currently
browsed `repo:owner/name` scope and ignores any typed `repo:` qualifier so
searches on those pages cannot escape the repository. The same parser powers
the search boxes, so examples such as `author:esp label:bug`, `commenter:mf`,
`milestone:"v1.0"`, `no:assignee`, `assignee:*`, `mentions:@me`, and
`created:2026-05-01..2026-05-19` work from the repo lists as well as the
global search page.

Profile and organization repository tabs also parse the same repository
qualifier subset, so `language:Go`, `topic:forge`, `is:public`,
`fork:false`, and `repo:owner/name` behave like GitHub's repository list
filters instead of becoming literal substring searches.

Global dashboard issue and pull-request pages (`/issues/*`, `/pulls`) also use
the shared parser for their filter box. Dashboard defaults are rendered as
GitHub-style queries, and user-entered qualifiers now execute instead of being
stripped to free text: `review-requested:@me` reads the pending
`pr_review_requests` table, `sort:comments-desc` orders by issue comment count,
and state tabs remove or override `state:` / `is:` operators without emitting
invalid `state:all` query text.

## Ranking

* **Repos**: `ts_rank_cd * (1 + ln(1 + star_count)) * recency_decay`
  where `recency_decay = 1 / (1 + days_since_update / 30)` (the
  spec's day-1 lean). Whole expression lives in SQL so Postgres
  short-circuits on the GIN index.
* **Issues**: `ts_rank_cd * state_weight` with `open = 1.5x` over
  `closed`. The spec doesn't pin the multiplier; 1.5 surfaces
  actionable issues first without burying closed history.
* **Users**: `ts_rank_cd` only. Suspended/deleted users are
  excluded at the WHERE clause so they never taint results.
* **Code**: path hits rank `+1.0` over content hits at the same
  `ts_rank_cd`. Within content hits, `ts_rank_cd` dominates
  trigram similarity.

## Code-search index lifecycle

* **Push trigger**: `push:process` enqueues `repo:index_code` when
  a push advances the repo's default branch. The job is idempotent
  + atomic-swap, so concurrent pushes that land while the previous
  index is running re-trigger on the next push tick.
* **Atomic swap**: the worker runs `DELETE … + INSERT …` for the
  repo in one tx. Readers never see a partial index.
* **Size + textness gates**:
  * Files > 256 KiB → path indexed, content skipped.
  * Files with NUL bytes in the first 8 KiB → treated as binary;
    path indexed, content skipped.
  * Indexed content truncated to 64 KiB so the trigram column
    doesn't bloat for huge text files.
* **Path skiplist**: `vendor/`, `node_modules/`, `dist/`, anything
  under `.git*` is skipped by default. `path:` and `extension:` filter the
  indexed path/content rows that are present; skipped directories remain
  absent until indexing policy changes.
* **Reconciler**: `repo:index_reconcile` enqueues a `repo:index_code`
  job for each repo where `default_branch_oid <> last_indexed_oid`.
  Self-throttling (100 repos per tick). Designed to run from cron
  every 5 minutes once the cron framework lands; for now it's
  invocable as a job.

## Routes

| Method | Path             | Notes                                      |
|--------|------------------|--------------------------------------------|
| GET    | `/search`        | Full results page with GitHub-style filters |
| GET    | `/search/quick`  | HTML fragment endpoint for top-bar drop     |

The top-bar nav embeds a search form pointing at `/search`; the
same input now calls `/search/quick` as the user types and renders
the returned fragment under the nav search box. Full-page type URLs
emit GitHub-style `type=repositories` and `type=pullrequests` while
still accepting the legacy `type=repos` and `type=pulls` aliases.

## What we deferred from the spec

* **Result-HTML caching with viewer-fingerprint key**: the spec's
  30-second cache + `(actor_id, repo_count, last_collab_change_at)`
  fingerprint scheme. The cache key correctness is fiddly enough
  that we want measurements before we ship it. Without the cache
  the per-query cost is dominated by GIN-index lookups, which are
  fast on the synthetic fixture and within budget. **Forward-deferred
  to S36 (perf pass)**.
* **API endpoint** `GET /api/v1/search?q=…&type=…`: the orchestrator
  is API-shaped; the handler wrap is a tiny add. Parking until the
  S33 webhooks sprint pulls in the rest of the API surface so we
  do them together (consistency on auth + body cap + scope shapes).
  **Forward-deferred to S33 / S34 API consolidation.**
These are all noted in the S28 status block as well.

## Pitfalls noted in code

* **Visibility leak** is the highest-stakes risk. The composer is
  the security boundary; the test suite asserts empty results for
  anon + non-collab against private fixtures.
* **`tsvector` size limits**: per-document content cap (64 KiB)
  defends.
* **Locale / accent**: `unaccent` is in the FTS config chain; tests
  cover.
* **Tokenizer breakdown on code**: trigram fallback exists in the
  schema (`content_trgm` + `gin_trgm_ops`); the SQL composes both
  the FTS hit and a future trigram-similarity hit (post-MVP — the
  v1 SearchCode runs FTS only, not trigram, because untyped trgm
  similarity needs a per-query threshold and we haven't chosen one
  yet).
* **Index drift**: triggers on issues/repos/users are reliable;
  code index relies on the worker. The reconciler is the safety
  net.
* **Suspended user content**: user-search excludes them via the
  WHERE clause. Issue/PR-search inherits their content via the
  underlying repo's visibility — that matches the spec's "visible
  inside their repo to collaborators" semantics.
