# Changelog

All notable changes to shithub are documented here. This project
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
conventions and [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Public docs site (`docs/public/`) built with mdBook.
- Contributor + security disclosure docs finalized for post-launch
  posture (DCO sign-off, security@ mailbox).
- Architecture overview + internal docs index.
- Operator runbooks: rotate-secrets, rotate-keys, regenerate-akc,
  drain-workers, read-only-mode.
- `THIRD_PARTY_NOTICES.md` with a CI-verified generator script.

### Changed
- README pivoted to post-launch framing (still flags WIP areas
  honestly).

## [1.0.0] — TBD

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

[Unreleased]: https://github.com/tenseleyFlow/shithub/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/tenseleyFlow/shithub/releases/tag/v1.0.0
