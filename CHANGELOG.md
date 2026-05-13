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
- **REST: commits (S50 §11).** Read-only git history surface:
  `GET /api/v1/repos/{o}/{r}/commits` (paginated; honours
  `?sha=`, `?path=`, `?author=`, `?since=`, `?until=`) and
  `GET /api/v1/repos/{o}/{r}/commits/{sha}` (full commit detail
  with committer/parents/tree + per-file `status` and
  `additions`/`deletions`, plus a rollup `stats` object). Backed
  by `internal/repos/git.Log` / `git.GetCommit` — the response
  stays in lock-step with the bare repository. Scope:
  `repo:read`. Empty repos return `[]`; the single-commit GET
  accepts any unambiguous SHA prefix.
- **Capability:** `commits` added to `/api/v1/meta`.
- **GPG keys + commit signature verification (S51).** Full
  vertical: upload OpenPGP public keys at
  `Settings → SSH and GPG keys`; shithub parses the armored
  block (RSA≥2048, ECC, multi-subkey, encryption-only all
  accepted) and stores primary + subkey fingerprint index. A
  background worker eagerly backfills verification rows for the
  uploader's existing commits across every repo, and the
  `shithubd gpg-backfill-all` admin subcommand performs the
  once-off bulk walk on deploy. Signed commits + annotated tags
  render a green **Verified** pill on the commit list,
  single-commit, and tag list pages; the popover surfaces the
  signer, key id, and verified-at timestamp. The `reason` enum
  mirrors GitHub's documented set (`valid`, `unsigned`,
  `unknown_key`, `bad_email`, `unverified_email`, `expired_key`,
  `not_signing_key`, `malformed_signature`, `invalid`). REST
  surface: `GET/POST/DELETE /api/v1/user/gpg_keys[/{id}]` with
  the gh-exact JSON shape (split `can_encrypt_comms` /
  `can_encrypt_storage`, both `public_key` and `raw_key`,
  subkeys-as-nested-objects, `emails[].verified` cross-checked
  against the user's verified-email set). Scopes: `user:read`
  for GETs, `user:write` for mutations. Existing
  `/api/v1/repos/{o}/{r}/commits[/{sha}]` responses now carry a
  `verification` object with the same shape gh emits.
  Migrations: `0068_user_gpg_keys.sql`,
  `0069_user_gpg_subkeys.sql`,
  `0070_commit_verification_cache.sql`.
- **Capability:** `gpg-keys` added to `/api/v1/meta`.
- **REST: rulesets (S50 §9).** Three read-only endpoints
  synthesizing GitHub's modern rulesets shape from shithub's
  existing `branch_protection_rules` rows:
  `GET /api/v1/repos/{o}/{r}/rulesets` (list),
  `GET /api/v1/repos/{o}/{r}/rulesets/{id}` (single),
  `GET /api/v1/repos/{o}/{r}/rules/branches/{branch}` (rules
  applying to a branch — every matching pattern, not just the
  longest-match the pre-receive enforcer uses). One protection
  row maps to one ruleset; each configured field projects as a
  typed rule (`pull_request`, `non_fast_forward`, `deletion`,
  `required_signatures`, `required_status_checks`). Scope:
  `repo:read`. Cross-repo lookups 404. Mutating rulesets via
  REST is a future surface — for now use the web UI at
  `Settings → Branches`.
- **Capability:** `rulesets` added to `/api/v1/meta`.
- **REST: repo contents (S50 §12).**
  `GET /api/v1/repos/{o}/{r}/contents/{path}[?ref=]` returns
  either a directory listing (dirs first, then files
  alphabetically) or a single file with base64-encoded
  `content`, `encoding`, `size`, and a `binary` flag (UTF-8
  validity check). Files over 1 MiB come back as
  `truncated: true` with empty content — clients fall through
  to the raw download path. Scope: `repo:read`. Empty `/contents`
  path returns the repo root.
- **Capability:** `contents` added to `/api/v1/meta`.
- **REST: forks (S50 §13).**
  `GET /api/v1/repos/{o}/{r}/forks` (paginated; per-row
  visibility filter so private forks of public repos only
  surface to authorized viewers) and
  `POST /api/v1/repos/{o}/{r}/forks` (fork into the
  authenticated user's namespace; optional `name`/`visibility`
  body). Reuses the existing `internal/repos/fork` orchestrator
  and enqueues the on-disk clone via the
  `repo:fork_clone` worker, so the response returns
  immediately with `init_status: "init_pending"`. Org-target
  forks land in a follow-up.
- **Capability:** `forks` added to `/api/v1/meta`.
- **REST: notifications (S50 §14).** User-scoped inbox surface:
  `GET /api/v1/notifications` (defaults to unread only;
  `?all=true` includes read; paginated with `Link:` headers),
  `GET /api/v1/notifications/threads/{id}` (single fetch with
  existence-leak-safe cross-user 404),
  `PATCH /api/v1/notifications/threads/{id}` (mark read/unread
  — empty body / `unread:false` → read; `unread:true` flips
  back), and `PUT /api/v1/notifications` (mark all read).
  Scopes: `user:read` on GETs, `user:write` on mutations. All
  routes are implicitly scoped to the authenticated user.
- **Capability:** `notifications` added to `/api/v1/meta`.
- **REST: watching / subscriptions (S50 §15).** Per-repo
  watch-level management mirroring GitHub's
  `/repos/{o}/{r}/subscription` shape:
  `GET .../subscribers` (paginated watcher list excluding
  `ignore` and suspended users), `GET .../subscription`
  (viewer's level + `explicit` flag distinguishing the
  implicit `participating` default from an explicit choice),
  `PUT .../subscription` (set `all` / `participating` /
  `ignore`), and `DELETE .../subscription` (revert to implicit).
  Reuses `internal/social.SetWatch` / `UnsetWatch` and
  `socialdb.ListWatchersForRepo`. Scope: `repo:read` on GETs,
  `user:write` on mutations.
- **Capability:** `watching` added to `/api/v1/meta`.
- **REST: events / activity (S50 §16).** Read-only activity feed
  over `domain_events`: `GET /api/v1/repos/{o}/{r}/events`
  (paginated; returns every event for the repo, gated by
  `ActionRepoRead`) and `GET /api/v1/users/{username}/events`
  (paginated; only `public=true` rows — matches gh, which never
  surfaces private-repo activity on a user feed). Scope:
  `repo:read` / `user:read`. Reuses the existing
  `socialdb.ListEventsForRepo` and
  `socialdb.ListPublicEventsForActor` queries.
- **Capability:** `events` added to `/api/v1/meta`.
- **REST: followers / following (S50 §17).**
  `GET /api/v1/users/{username}/followers` and
  `GET /api/v1/users/{username}/following` (both paginated;
  `Link:` headers), plus the authenticated-user-scoped
  `GET /api/v1/user/following/{target}` (204/404 membership
  probe matching gh), `PUT /api/v1/user/following/{target}`
  (follow), and `DELETE` (unfollow). Self-follow returns 422.
  Reuses `internal/social.FollowUser` / `UnfollowUser` so the
  follow rate-limit and `followed_user` domain event stay in
  one place. Org-follow variants remain on the HTML surface.
- **Capability:** `followers` added to `/api/v1/meta`.
- **REST: actions workflow runs (S50 §18).** Read-only access to
  the Actions run history:
  `GET /api/v1/repos/{o}/{r}/actions/runs` (paginated; filterable
  by `workflow_file`, `head_ref`, `actor`, `event`, `status`,
  `conclusion`),
  `GET /api/v1/repos/{o}/{r}/actions/runs/{run_id}` (single
  run; existence-leak-safe cross-repo 404), and
  `GET /api/v1/repos/{o}/{r}/actions/runs/{run_id}/jobs`
  (job-index-ordered jobs list with `needs_jobs` graph). Reuses
  the existing `actionsdb.ListWorkflowRunsForRepo` /
  `GetWorkflowRunByID` / `ListJobsForRun` queries. Scope:
  `repo:read`. Lifecycle controls (cancel / rerun / approve)
  remain on the actions-lifecycle routes; this surface is
  read-only.
- **Capability:** `actions-runs` added to `/api/v1/meta`.
- **REST: stargazers + starred lists (S50 §19).**
  `GET /api/v1/repos/{owner}/{repo}/stargazers` paginates the users
  who starred a repo (scope: `repo:read`); private-repo lists are
  gated by `ActionRepoRead`. `GET /api/v1/users/{username}/starred`
  paginates the repos a user has starred (scope: `user:read`);
  cross-user views post-filter private repos the caller can't see.
  Both endpoints emit standard `Link:` pagination headers and
  recency-sort by `starred_at DESC`. The S26 caller-self star
  routes (`/api/v1/user/starred` and `/api/v1/user/starred/{o}/{r}`)
  are unchanged.
- **Capability:** `stargazers` added to `/api/v1/meta`.
- **REST: issue events / timeline (S50 §20).**
  `GET /api/v1/repos/{owner}/{repo}/issues/{number}/events` returns
  the issue's recorded timeline (every `closed` / `reopened` /
  `labeled` / `unlabeled` / `milestoned` / `demilestoned` /
  `locked` / `unlocked` / `referenced` / merged / push event), with
  `actor_username` LEFT-joined and the raw event `meta` payload
  preserved verbatim. Paginated with the standard `Link:` headers,
  sorted oldest-first. Scope: `repo:read`.
- **Capability:** `issue-events` added to `/api/v1/meta`.
- **Device-code login (S50 §1, RFC 8628).**
  `POST /login/device/code` issues a fresh authorization grant for
  a non-browser client (CLI / TV / IoT). `POST /login/oauth/access_token`
  polls for the user's approval and, on success, mints a PAT bound
  to the requested scopes. The browser-facing verification page is
  served at `GET /login/device` (CSRF-protected). The matching CLI
  endpoints are CSRF-exempt. `client_id` is enforced against an
  allowlist (default: `shithub-cli`); requested scopes go through
  the standard `pat.ValidScope` filter so unknown scopes fail
  cleanly with `invalid_scope`. The minted PAT is disclosed exactly
  once — subsequent exchanges of the same `device_code` return
  `invalid_grant` even after successful approval. RFC 8628 §3.5
  error semantics (`authorization_pending`, `slow_down`,
  `access_denied`, `expired_token`) are honored.
- **Capability:** `device-code` added to `/api/v1/meta`.
- **REST: actions workflows + workflow_dispatch (S50 §13 part 1).**
  `GET /api/v1/repos/{o}/{r}/actions/workflows` lists the workflows
  discovered in `.shithub/workflows/` at the repo's default-branch
  HEAD (or `?ref=`); `GET .../workflows/{id_or_file}` fetches a
  single workflow by basename, full path, or a deterministic
  64-bit hash of the path. `POST .../workflows/{file}/dispatches`
  triggers `workflow_dispatch` with optional `ref` and a typed
  `inputs` map (choice / boolean / string with required/default
  enforcement, same semantics as the HTML "Run workflow" button).
  Workflow_dispatch input validation now lives in a shared
  `internal/actions/dispatch` package consumed by both the REST
  and HTML surfaces. Enable/disable knobs are deferred to a
  follow-up (needs a new `workflow_disabled` table); every listed
  workflow is reported as `state: "active"` for now.
- **Capability:** `actions-workflows` added to `/api/v1/meta`.
- **REST: actions lifecycle + artifacts + job logs (S50 §13 part 2).**
  New migration `0064_workflow_disabled` adds a per-workflow
  disable knob; `trigger.Enqueue` now consults it and short-circuits
  matching events for disabled workflows. The list endpoint surfaces
  this as `"state": "disabled"`. REST additions:
  `PUT /api/v1/repos/{o}/{r}/actions/workflows/{file}/enable` and
  `.../disable`,
  `DELETE /api/v1/repos/{o}/{r}/actions/runs/{run_id}` (cascades
  through jobs/steps/log-chunks/artifacts; object-store cleanup is
  async best-effort),
  `GET /api/v1/repos/{o}/{r}/actions/runs/{run_id}/artifacts` +
  `GET .../actions/artifacts/{aid}` +
  `GET .../actions/artifacts/{aid}/zip` (streams from object store) +
  `DELETE .../actions/artifacts/{aid}`,
  `GET /api/v1/repos/{o}/{r}/actions/jobs/{job_id}/logs` (assembled
  transcript with `##[group]`/`##[endgroup]` step markers).
- **Capabilities:** `actions-artifacts`, `actions-job-logs` added to
  `/api/v1/meta`.
- **REST: actions secrets + variables (S50 §13 part 3).**
  New `internal/auth/sealbox` package owns the server's X25519
  keypair and exposes NaCl `SealedBox` decode. Clients fetch the
  public key via
  `GET /api/v1/repos/{o}/{r}/actions/secrets/public-key` (and the
  org analog), encrypt the secret value with `crypto_box_seal`, and
  PUT the base64 ciphertext + key_id. Server validates key_id,
  decrypts in memory, then re-encrypts with the shared storage
  AEAD for at-rest persistence — plaintext never reaches postgres.
  Secrets CRUD: `GET .../secrets`, `GET .../secrets/{name}`,
  `PUT .../secrets/{name}`, `DELETE .../secrets/{name}`. Variables
  CRUD (plaintext, `${{ vars.NAME }}` namespace): `GET/POST
  .../variables`, `GET/PATCH/DELETE .../variables/{name}`. Both
  surfaces have repo- and org-scoped variants. Operator setup:
  `SHITHUB_ACTIONS__SECRETS__BOX_PRIVATE_KEY_B64` carries the
  X25519 private key (32 bytes base64); unset means
  auto-generated-per-process with a loud warning at startup, which
  is dev-only behavior.
- **Capabilities:** `actions-secrets`, `actions-variables` added to
  `/api/v1/meta`.
- **REST: actions caches (S50 §13 part 4 — §13 closure).** New
  migration `0065_workflow_caches` adds the `workflow_caches` table
  keyed on `(repo_id, cache_key, cache_version, git_ref)` with
  size + last_accessed_at + object_key columns and a unique
  constraint. REST surface:
  `GET /api/v1/repos/{o}/{r}/actions/caches` (paginated; optional
  `?key=` and `?ref=` filters; standard `Link:` headers; sorted
  recency-DESC by `last_accessed_at`),
  `DELETE .../actions/caches/{cache_id}` (single delete with
  cross-repo 404 guard + best-effort async S3 cleanup),
  `DELETE .../actions/caches?key=...[&ref=...]` (bulk delete by
  key; idempotent — 204 even with zero matches). The runner-side
  upload protocol that POPULATES this table is a future sprint;
  this REST surface lands first so operators have an audit + purge
  seat for when caches arrive.
- **Capability:** `actions-caches` added to `/api/v1/meta`.
- **REST: repos follow-ups (S50 §2 closure).**
  `GET /api/v1/repos/{o}/{r}/readme[?ref=]` returns the repo's
  README as a base64-encoded blob with `download_url` for the raw
  bytes (capped at 1 MiB to match the HTML render cap; prefers
  `.md`/`.markdown` over plain text when multiple READMEs exist).
  `PUT /api/v1/repos/{o}/{r}/topics` and
  `DELETE .../topics` replace and clear the topic set atomically
  (server-side normalization: lowercase, dedup; constraints: max
  20, 1–50 chars of `[a-z0-9-]`). `POST .../merge-upstream`
  fast-forwards a fork's default branch to its upstream — refuses
  non-forks with 422 and divergent forks with 409 (users must
  reconcile locally). Scopes: `repo:read` on README,
  `repo:write` on topics and merge-upstream.
- **Capabilities:** `readme`, `topics`, `merge-upstream` added to
  `/api/v1/meta`.

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
