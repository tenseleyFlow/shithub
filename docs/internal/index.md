# Internal docs index

These docs live next to the code; they're for the people running
shithub the project (operators + contributors). The public-facing
subset is mirrored under `docs/public/` and built into the docs
site.

> **Convention.** Internal docs answer "why" and link to the
> code. Public docs answer "how do I" and explain only what a
> user/operator needs.

## Architecture & wiring

- [architecture.md](./architecture.md) — system overview, request
  lifecycle, data flow, deployment topology.
- [config.md](./config.md) — config loader, key reference, secrets
  handling.
- [db.md](./db.md) — schema overview, migration tooling.
- [db-indexes.md](./db-indexes.md) — index catalog with the
  rationale per index.
- [db-roles.md](./db-roles.md) — Postgres role/grant scheme.
- [observability.md](./observability.md) — logging, tracing,
  metrics, error reporting.
- [storage.md](./storage.md) — bare-repo + object store.
- [worker.md](./worker.md) — job queue + handlers.
- [caching.md](./caching.md) — LRU + singleflight + cache
  invariants.
- [bench.md](./bench.md) — bench harness usage + N+1 audit
  pattern.

## Auth & security

- [auth.md](./auth.md) — sessions, password, TOTP, recovery
  codes.
- [tokens.md](./tokens.md) — PAT format, scopes, revocation.
- [permissions.md](./permissions.md) — `policy.Can` semantics.
- [2fa.md](./2fa.md) — TOTP enrollment, recovery flow.
- [ssh-deploy.md](./ssh-deploy.md) — AKC contract.
- [git-ssh.md](./git-ssh.md), [git-http.md](./git-http.md) — git
  transports.
- [security-checklist.md](./security-checklist.md) — controls + the
  tests proving them.
- [threat-model.md](./threat-model.md) — v1 attacker model.
- [hooks.md](./hooks.md) — pre/post-receive contracts.

## Domain features

- [profile.md](./profile.md), [settings.md](./settings.md)
- [repo-create.md](./repo-create.md),
  [repo-lifecycle.md](./repo-lifecycle.md)
- [code-tab.md](./code-tab.md), [diffs.md](./diffs.md),
  [commits-blame.md](./commits-blame.md)
- [forks.md](./forks.md), [stars-watchers.md](./stars-watchers.md)
- [issues.md](./issues.md), [pull-requests.md](./pull-requests.md),
  [pr-review.md](./pr-review.md)
- [branch-protection.md](./branch-protection.md),
  [checks.md](./checks.md)
- [actions-schema.md](./actions-schema.md),
  [actions-runner-api.md](./actions-runner-api.md)
- [orgs.md](./orgs.md), [teams.md](./teams.md)
- [notifications.md](./notifications.md)
- [search.md](./search.md), [markdown.md](./markdown.md)
- [seo.md](./seo.md) — crawler endpoints, metadata, sitemap, and
  public positioning.

## Operations

- [deploy.md](./deploy.md) — Ansible playbook + topology.
- [runbooks/runner-deploy.md](./runbooks/runner-deploy.md) —
  Actions runner host deployment.
- [runbooks/](./runbooks/) — incident, backup, restore, upgrade,
  rollback, plus rotation procedures.

## Conventions for adding a new doc

1. One markdown file per subsystem; name it after the subsystem,
   not the sprint.
2. Link to the relevant package directory in the first paragraph.
3. Public-facing material that should appear on
   [docs.shithub.tld](https://docs.shithub.tld) goes in
   `docs/public/...` and follows the operator/user voice; the
   internal version is the authoritative source on "why".
4. Update this index when you add a new file.
