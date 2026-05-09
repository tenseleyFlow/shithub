# Changelog

All notable changes to shithub are documented here. This project
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
conventions and [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Pre-1.0 versioning: minor versions may break the API. The
stability contract begins at v1.0.0; until then, expect changes
between minor releases.

## [Unreleased]

(Empty — first post-launch entries land here.)

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
