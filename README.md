<p align="center">
  <img src="internal/web/static/logo/shithub-mark.svg" alt="" width="120">
</p>

<h1 align="center">shithub</h1>

<p align="center">
  A 1:1 reverse-engineering of GitHub. AGPLv3. Without Copilot.
</p>

---

**Honest status: very much a work in progress.**

shithub is an attempt to recreate GitHub — the platform, the UI, the workflows — as faithfully as we can, as a self-hostable open-source forge. The goal is "you should barely notice you switched." We are nowhere near that yet. It will be some time before it looks and feels the way GitHub does. There is a lot of GitHub.

If you came here expecting drop-in parity, it's not ready for you. If you came here to watch (or help) one get built in the open, welcome.

## What works today

The core forge loop works end-to-end against the codebase you're reading:

- **Identity** — email/password signup, email verification, password reset, TOTP 2FA + recovery codes, SSH keys, personal access tokens (scoped), sessions.
- **Repositories** — create, fork, archive, transfer, soft-delete with grace, rename with redirects, visibility toggles, branch protection (force-push / deletion / required reviews / required status checks), default-branch swap, topics, README/license/.gitignore templates.
- **Git** — bare repos on disk; HTTPS smart-HTTP push/pull (SSH service is planned but not shipped yet); pre/post-receive hook integration for size accounting and event emission.
- **Code browsing** — tree, blob (chroma syntax highlighting with light/dark themes), raw, blame, commit history, individual commit views, branch/tag listings, compare views, file finder.
- **Issues & PRs** — full CRUD on issues + comments + labels + milestones + assignees; pull requests with diff rendering, file-by-file review, line comments, reviews (approve/request-changes/comment), required-reviewer enforcement, status-check gates, three merge methods (merge / squash / rebase), conflict detection.
- **Social** — stars, watches with notification level, forks (clone-on-create), `/explore`, stargazer/watcher lists.
- **Search** — code search (path + content, tantivy-equivalent indexing in Postgres), repo search, user search, quick-search.
- **Notifications** — per-user inbox, email fan-out, watch-level routing, one-click HMAC-signed unsubscribe links, auto-subscribe on participation.
- **Organizations & teams** — create, member roles (member/owner), invitations, one-level team nesting, team grants on repos with max-of-sources policy aggregation.
- **Repo settings** — General (description, topics, features, merge methods), Access (collaborators + team grants), Branches (protection rules), Danger (rename/transfer/archive/visibility/delete).
- **Webhooks** — outbound delivery with HMAC-SHA256 signing, exponential backoff with jitter, auto-disable on persistent failure, SSRF defense (DNS resolve + IP block-list + dial-the-IP transport, no redirect-following), redelivery UI, ping events.

## What doesn't work yet

Pulled directly from the sprint plan we're working through:

- **SSH git service** — HTTPS works; the SSH front-end is planned. Use HTTPS clone URLs for now.
- **Actions / CI** — there is no CI runner. Status checks are wired into PR gates so a future runner can publish into them.
- **Packages, Pages, Projects, Releases, Gists** — none of these surfaces exist yet.
- **GraphQL API** — only the internal HTTP surface exists. There is no public REST or GraphQL API.
- **Activity feed** — domain events are recorded but the activity-feed view isn't built.
- **Admin / site-admin surface** — there is no `/admin` UI. Operator tooling is via `shithubd` subcommands and SQL.
- **Visual polish** — most pages render correctly but do not look like GitHub yet. Spacing, typography, header/footer chrome, color tokens, octicon coverage, empty-state illustrations, focus states — all still drifting from Primer. Closing this gap is ongoing; expect rough edges page-to-page.
- **Mobile / responsive** — the current CSS is desktop-first. Small viewports work but aren't tuned.
- **Deferred from completed sprints** — every sprint spec lists "inbound deferrals" that landed and "outbound deferrals" that were pushed forward. See `.docs/sprints/` for the running list.

## Why

GitHub is a well-built platform. Microsoft's Copilot push has changed the product's character — particularly around how user code is treated as training data. shithub aims to recreate the parts that worked, on terms that make a self-hosted instance an honest equal to the SaaS experience, with AGPLv3 keeping any hosted variant open.

The "1:1" goal is intentional. There are plenty of git forges with their own UX vision; shithub deliberately doesn't try to be one of them. The closer the muscle-memory match, the lower the cost of switching.

## Quickstart (development)

```sh
make dev          # hot-reload server on http://localhost:8080
make test         # run tests
make build        # build bin/shithubd
make ci           # full CI pipeline locally (mirrors GitHub Actions)
make dev-migrate  # apply DB migrations against $SHITHUB_DATABASE_URL
```

Requires:
- Go 1.22+ (developed against 1.26)
- PostgreSQL 13+
- A local MinIO (or any S3-compatible store) for the storage tests — `make storage-up` brings one up via docker-compose.
- `golangci-lint`, `gofumpt`, `goimports`, `air`, `sqlc`, `goose` (installed via `go install` per the Makefile or your package manager).

Copy `.env.example` to `.env` and fill in the database URL, session keys, and TOTP/webhook AEAD key.

## Layout

```
cmd/shithubd/        # main entry: web, ssh, worker, migrate, storage, version
internal/auth/       # password, sessions, TOTP, PATs, SSH keys, policy/RBAC
internal/repos/      # repo lifecycle, git ops, branches, tags, diffs, forks
internal/issues/     # issues + comments + labels + milestones
internal/pulls/      # pull requests + reviews + merge orchestration
internal/orgs/       # orgs + teams + member roles + principals
internal/webhook/    # outbound delivery + SSRF defense + signing
internal/notif/      # inbox + email fan-out + watch-level routing
internal/search/     # code/repo/user search
internal/markdown/   # rendering + reference resolution (#N, @user, @org/team)
internal/web/        # HTTP handlers + html/template + middleware
internal/worker/     # Postgres-backed job queue + handlers
internal/migrationsfs/migrations/   # numbered SQL migrations (goose)
.docs/sprints/       # sprint specifications (S00–S49 + SR1)
```

## Roadmap

The work is structured as 50 sprints (S00–S49, plus SR1 audit remediation). Roughly:

- **S00–S04** — scaffolding, DB baseline, web shell, observability, storage abstractions ✅
- **S05–S09** — identity (auth, 2FA, SSH keys, PATs, profiles) ✅
- **S10–S20** — repos, git protocols, code browsing, branches/tags, hooks ✅ (SSH service deferred)
- **S21–S25** — issues, pulls, reviews, status checks, markdown ✅
- **S26–S29** — social, forks, search, notifications ✅
- **S30–S33** — orgs, teams, repo settings, webhooks ✅ (current)
- **S34–S40** — site admin, security hardening, federation, polish, accessibility, i18n
- **S41–S49** — actions/CI, social feed, advanced search, GraphQL API, packages, pages, projects, releases, gists

See `.docs/sprints/` for the per-sprint spec.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The project is in active rapid development — substantial PRs are best discussed in an issue first so we don't both write the same code. Small fixes, doc improvements, and bug reports are welcome any time.

## Security

To report a security issue, see [SECURITY.md](SECURITY.md). Please do not open a public issue for security reports.

## License

[AGPLv3](LICENSE). If you run a modified version as a network service, your users are entitled to the source.
