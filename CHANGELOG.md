# Changelog

All notable changes to shithub are documented here. This project
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
conventions and [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Pre-1.0 versioning: minor versions may break the API. The
stability contract begins at v1.0.0; until then, expect changes
between minor releases.

## [Unreleased]

### Added

- **REST API contract (S50 §0).** `GET /api/v1/meta` returns the
  server's version stamp and a list of feature capability strings
  for client-side feature detection. Every `/api/v1/*` response
  now carries `X-RateLimit-Limit`, `X-RateLimit-Remaining`,
  `X-RateLimit-Reset`, and (when PAT-authenticated) `X-OAuth-Scopes`.
  The 403 scope-reject response also carries
  `X-Accepted-OAuth-Scopes`. Operators tune the API rate-limit
  budgets via `ratelimit.api.authed_per_hour` /
  `ratelimit.api.anon_per_hour` (defaults: 5000 / 60).
- **Pagination helper** `internal/web/handlers/api/apipage` —
  emits canonical RFC 8288 Link headers (`first`/`prev`/`next`/`last`)
  with absolute URLs rooted at the configured public base URL.
- **REST: user emails (S50 §1).** `GET /api/v1/user/emails` lists
  the authenticated user's emails. Optional `?verified=true|false`
  filter. Scope: `user:read`.
- **REST: user SSH keys (S50 §1).** `GET/POST /api/v1/user/keys`
  and `GET/DELETE /api/v1/user/keys/{id}` expose CRUD for git
  authentication keys. Signing keys are tracked separately by a
  new `kind` column on `user_ssh_keys` and remain on the HTML
  surface for now. Scopes: `user:read` for GETs, `user:write` for
  mutations.
- **Capabilities:** `user-emails`, `ssh-keys` added to
  `/api/v1/meta` response.
- **REST: repos core (S50 §2).**
  `GET /api/v1/user/repos`, `GET /api/v1/users/{username}/repos`,
  `GET /api/v1/orgs/{org}/repos`,
  `GET /api/v1/repos/{owner}/{repo}`,
  `POST /api/v1/user/repos`,
  `POST /api/v1/orgs/{org}/repos`,
  `PATCH /api/v1/repos/{owner}/{repo}` (description, has_issues,
  has_pulls, archived, visibility), and
  `DELETE /api/v1/repos/{owner}/{repo}` (soft-delete).
  Visibility-aware listing: a user's `/users/{u}/repos` shows
  private rows only to that user; an org's `/orgs/{o}/repos`
  shows private rows only to members. Single-repo GETs `404`
  for callers who can't see the row (no existence leak).
- **Capability:** `repos` added to `/api/v1/meta`.
- **REST: issues + comments + lock (S50 §3).**
  `GET /api/v1/repos/{o}/{r}/issues` (with `?state=` filter and
  `Link:`-header pagination),
  `GET /api/v1/repos/{o}/{r}/issues/{number}`,
  `POST /api/v1/repos/{o}/{r}/issues`,
  `PATCH /api/v1/repos/{o}/{r}/issues/{number}` (title/body
  author-gated, state/state_reason policy-gated),
  `GET / POST /api/v1/repos/{o}/{r}/issues/{number}/comments`,
  `PATCH / DELETE /api/v1/repos/{o}/{r}/issues/comments/{cid}`,
  `PUT / DELETE /api/v1/repos/{o}/{r}/issues/{number}/lock`.
- **REST: repo labels (S50 §3).**
  `GET / POST /api/v1/repos/{o}/{r}/labels` and
  `GET / PATCH / DELETE /api/v1/repos/{o}/{r}/labels/{name}`.
- **Capabilities:** `issues`, `labels` added to `/api/v1/meta`.
- **REST: milestones + assignees (S50 §3 follow-up).** Full CRUD
  for `/api/v1/repos/{o}/{r}/milestones` (with `?state=` filter
  on list and live `open_issues`/`closed_issues` counters on
  every response), plus `GET /api/v1/repos/{o}/{r}/assignees`
  (repo owner + collaborators eligible for issue assignment).
  Scope: `repo:read` on GETs, `repo:write` on mutations.
  Mutations gate on `ActionIssueLabel`.
- **Issue PATCH extensions.** `PATCH /api/v1/repos/{o}/{r}/issues/{n}`
  now accepts `labels`, `assignees`, and `milestone` fields with
  GitHub-style full-replace semantics. Each gates on its own
  policy action (`ActionIssueLabel` / `ActionIssueAssign`) so a
  caller missing one capability gets a clean 403 rather than a
  partial update. Unknown label names or assignee usernames →
  422; cross-repo milestone ids → 422.
- **Capabilities:** `milestones`, `assignees` added to
  `/api/v1/meta`.
- **Reach:** `internal/web/handlers/api.resolveAPIRepo` now
  resolves both user-owner and org-owner repos — check-runs and
  every later batch implicitly gain org-repo support.

### Added (internal)

- **REST: pull requests core (S50 §4).**
  `GET /api/v1/repos/{o}/{r}/pulls` with `?state=` and `?draft=`
  filters,
  `GET /api/v1/repos/{o}/{r}/pulls/{number}`,
  `POST /api/v1/repos/{o}/{r}/pulls`,
  `PATCH /api/v1/repos/{o}/{r}/pulls/{number}` (title/body
  author-gated, state via `ActionPullClose`, draft→ready
  author-only),
  `GET /api/v1/repos/{o}/{r}/pulls/{number}/commits`,
  `GET /api/v1/repos/{o}/{r}/pulls/{number}/files`,
  `PUT /api/v1/repos/{o}/{r}/pulls/{number}/merge` (honoring
  the repo's default merge method and the optional `sha`
  head guard). Reviews + comments + reviewers + update-branch +
  auto-merge land in a follow-up.
- **Capability:** `pulls` added to `/api/v1/meta`.
- **REST: PR reviews + inline comments + requested reviewers (S50 §4b).**
  `GET / POST /api/v1/repos/{o}/{r}/pulls/{number}/reviews`
  (events `APPROVE` / `REQUEST_CHANGES` / `COMMENT`, with
  pending-draft attachment on submit),
  `GET / POST /api/v1/repos/{o}/{r}/pulls/{number}/comments`
  (inline review comments with file_path / side / position
  anchoring, `pending` drafts, `in_reply_to_id` threading), and
  `GET / POST / DELETE /api/v1/repos/{o}/{r}/pulls/{number}/requested_reviewers`
  (by `user_id` or `username`).
- **Capability:** `pr-reviews` added to `/api/v1/meta`.
- **REST: search (S50 §5).** `GET /api/v1/search/repositories`,
  `GET /api/v1/search/issues?type=issue|pr`, and
  `GET /api/v1/search/code` over the existing FTS corpus.
  Canonical gh-shaped envelope `{ total_count,
  incomplete_results, items }` with `Link:` pagination.
  Anonymous callers allowed (visibility filter inside the search
  package narrows to public). `?q=` honors the existing operator
  vocabulary (`repo:`, `is:`, `state:`, `author:`, phrase). The
  `search/commits` and `search/users` endpoints, plus the
  `sort=`/`order=` knobs, are deferred to follow-ups.
- **Capability:** `search` added to `/api/v1/meta`.
- **REST: orgs (S50 §7).** `GET /api/v1/user/orgs` (self),
  `GET /api/v1/users/{username}/orgs` (public; shithub has no
  hidden membership distinction in v1),
  `GET /api/v1/orgs/{org}` (single fetch; 404 for soft-deleted),
  `GET /api/v1/orgs/{org}/members`. Scope: `user:read`.
- **Capability:** `orgs` added to `/api/v1/meta`.
- **REST: repo webhooks (S50 §8).** Full CRUD over a repo's
  webhook subscriptions: `GET/POST /api/v1/repos/{o}/{r}/hooks`,
  `GET/PATCH/DELETE /api/v1/repos/{o}/{r}/hooks/{id}`. Deliveries
  read-side: `GET /api/v1/repos/{o}/{r}/hooks/{id}/deliveries`
  (paginated; `Link:` headers) and
  `GET /api/v1/repos/{o}/{r}/hooks/{id}/deliveries/{did}` (full
  transcript). `POST .../deliveries/{did}/redeliver` re-enqueues.
  Scope: `repo:write`; role floor: settings:general. Webhook
  secrets are write-only — set on create, rotated via PATCH's
  `secret` field, never echoed back. Create-time SSRF gate
  rejects loopback / private / disallowed-port targets so
  misconfigurations surface synchronously instead of as silent
  delivery failures.
- **Capability:** `webhooks` added to `/api/v1/meta`.
- **REST: branches + tags (S50 §9).** Read-only ref enumeration:
  `GET /api/v1/repos/{o}/{r}/branches` (paginated; each entry
  carries `protected` reflecting the longest-prefix match against
  the configured branch-protection rules, plus `is_default`),
  `GET /api/v1/repos/{o}/{r}/branches/{name}` (slashes in branch
  names accepted verbatim or URL-encoded), and
  `GET /api/v1/repos/{o}/{r}/tags` (paginated). Scope: `repo:read`.
  Empty / uninitialised repos return `[]` rather than `404`.
- **Capabilities:** `branches`, `tags` added to `/api/v1/meta`.
- **REST: repo collaborators (S50 §10).**
  `GET /api/v1/repos/{o}/{r}/collaborators` (list),
  `GET .../collaborators/{username}` (204 membership probe),
  `GET .../collaborators/{username}/permission` (permission
  level — `"none"` when not a collaborator),
  `PUT .../collaborators/{username}` (add / upgrade, body
  `{"role": "..."}` accepting both shithub names and gh-style
  aliases `pull`/`push`),
  `DELETE .../collaborators/{username}` (remove). Scope:
  `repo:read` on GETs, `repo:write` on mutations; mutations
  layer `ActionRepoAdmin` on top. Refuses (422) to enrol the
  repo owner.
- **Capability:** `collaborators` added to `/api/v1/meta`.

### Added (internal)

- `issues.Edit` orchestrator wraps `UpdateIssueTitleBody` with
  markdown re-render + cross-reference re-indexing. Used by the
  new PATCH-issue endpoint; available for the HTML edit flow when
  it lands.

### Changed

- **JSON error envelope on `/api/v1/*`.** `401` and `403`
  responses now emit `{"error": "..."}` with
  `Content-Type: application/json` (previously `text/plain`).
  Existing `4xx`/`5xx` responses from the handler bodies are
  unchanged.

## [0.1.0] — TBD (operator fills in cutover date)

The first public release of shithub. Pre-1.0: there is no
backward-compatibility promise yet. Migrations are forward-only;
schema may change between minor versions.

### Initial public surface

- **Identity** — signup, email verification, password reset, TOTP
  2FA + recovery codes, SSH keys, scoped PATs, sessions with
  per-account epoch invalidation.
- **Repositories** — create, fork, archive, transfer, soft-delete
  with grace, rename with redirects, visibility toggles, branch
  protection, default-branch swap, topics, README/license/
  .gitignore templates.
- **Git** — bare repos on disk; HTTPS smart-HTTP push/pull;
  pre/post-receive hook integration.
- **Code browsing** — tree, blob (chroma syntax highlighting),
  raw, blame, commit history, individual commit views, branch/tag
  listings, compare views, file finder.
- **Issues + PRs** — full CRUD; reviews; required-reviewer
  enforcement; status-check gates; three merge methods.
- **Social** — stars, watches, forks, `/explore`, stargazer/
  watcher lists.
- **Search** — code, repo, user, issue.
- **Notifications** — in-app inbox, email fan-out, one-click
  unsubscribe.
- **Orgs + teams** — roles, invitations, one-level nesting,
  max-of-sources policy.
- **Webhooks** — HMAC-signed delivery, exponential backoff,
  auto-disable, SSRF defense, redelivery UI.
- **Observability** — structured logs, Prometheus metrics,
  optional OTel tracing, Sentry-protocol error reporting.
- **Operations** — Ansible playbook, systemd units, Caddy edge,
  WireGuard mesh for monitoring, Postgres WAL archive + daily
  logical backups to Spaces, cross-region DR, restore drill.
- **Public landing page** on `/` for anonymous viewers; signed-in
  viewers get a quick-link dashboard.
- **Lightweight status page** at `docs.<host>/status.html`.
- **Cutover artifacts** under `deploy/cutover/`.
- **Public docs site** built with mdBook.
- **Operator runbooks** for incidents, backups, restore, upgrade,
  rollback, rotate-secrets, rotate-keys, regenerate-akc,
  drain-workers, read-only-mode, day-one.
- **a11y tooling** (pa11y + axe) and **k6 load-test scenarios**.
- **THIRD_PARTY_NOTICES.md** with a CI-verified generator.

### Known gaps at v0.1.0

- SSH git transport (HTTPS only)
- Actions / CI runner
- Packages, Releases, Pages, Projects, Gists
- GraphQL API (only a small REST surface today)
- Activity feed UI

These are all on the post-MVP roadmap.

[Unreleased]: https://shithub.sh/shithub/shithub/compare/v0.1.0...trunk
[0.1.0]: https://shithub.sh/shithub/shithub/releases/tag/v0.1.0
