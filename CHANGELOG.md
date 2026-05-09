# Changelog

All notable changes to shithub are documented here. This project
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
conventions and [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

(Empty — first post-launch entries land here.)

## [1.0.0] — TBD (operator fills in cutover date)

The first stable release. **Stability contract:** every migration
from this point on is backward-compatible from v1.0.0 — see
`docs/internal/runbooks/upgrade.md`.

### Added (since pre-launch)
- Public landing page on `/` for anonymous viewers; signed-in
  viewers get a quick-link dashboard.
- Lightweight status page at `docs.shithub.example/status.html`.
- Cutover artifacts under `deploy/cutover/` — checklist, smoke
  script, rollback script.
- Launch announcement copy at `docs/blog/v1.0.0-launch.md`.
- Day-one operator runbook at `docs/internal/runbooks/day-one.md`.
- Public docs site (`docs/public/`) built with mdBook.
- Contributor + security disclosure docs finalized for post-launch
  posture (DCO sign-off, `security@shithub.example` mailbox).
- Architecture overview + internal docs index.
- Operator runbooks: rotate-secrets, rotate-keys, regenerate-akc,
  drain-workers, read-only-mode.
- `THIRD_PARTY_NOTICES.md` with a CI-verified generator script.
- a11y tooling (pa11y + axe) and k6 load-test scenarios under
  `tests/`.

### Changed
- README pivoted to post-launch framing (still flags WIP areas
  honestly).
- Renderer (`internal/web/render/render.go`) walks `_*.html`
  partials recursively and fails loud on undefined template refs
  at startup.
- Repo Code view restructured to GitHub's 2/3 + 1/3 layout with
  an About sidebar (description, topics, license, language,
  star/watch/fork counts).

## [1.0.0] — core forge loop

The first stable release. Core forge loop:

- Identity: signup, email verification, password reset, TOTP 2FA
  + recovery codes, SSH keys, scoped PATs, sessions with
  per-account epoch invalidation.
- Repositories: create, fork, archive, transfer, soft-delete with
  grace, rename with redirects, visibility toggles, branch
  protection (force-push / deletion / required reviews / required
  status checks), default-branch swap, topics, README/license/
  .gitignore templates.
- Git: bare repos on disk; HTTPS smart-HTTP push/pull; pre/post-
  receive hook integration for size accounting and event emission.
- Code browsing: tree, blob (chroma syntax highlighting with
  light/dark themes), raw, blame, commit history, individual
  commit views, branch/tag listings, compare views, file finder.
- Issues + PRs: full CRUD; pull requests with diff rendering,
  file-by-file review, line comments, reviews, required-reviewer
  enforcement, status-check gates, three merge methods.
- Social: stars, watches with notification level, forks
  (clone-on-create), `/explore`, stargazer/watcher lists.
- Search: code, repo, user.
- Notifications: per-user inbox + email fan-out, watch-level
  routing, one-click HMAC-signed unsubscribe.
- Organizations + teams: create, member roles, invitations,
  one-level team nesting, team grants on repos with
  max-of-sources policy.
- Webhooks: outbound delivery with HMAC-SHA256 signing,
  exponential backoff with jitter, auto-disable on persistent
  failure, SSRF defense, redelivery UI, ping events.
- Observability: structured logs, Prometheus metrics, optional
  OTel tracing, Sentry-protocol error reporting.
- Security: AGPLv3, threat model + security checklist, package
  boundary lints (markdown, policy, secret-logs, CSRF).
- Operations: Ansible playbook, systemd units, Caddy edge,
  WireGuard mesh for monitoring, Postgres WAL archive + daily
  logical backups to Spaces, cross-region DR, restore drill.

[Unreleased]: https://shithub.example/shithub/shithub/compare/v1.0.0...trunk
[1.0.0]: https://shithub.example/shithub/shithub/releases/tag/v1.0.0
