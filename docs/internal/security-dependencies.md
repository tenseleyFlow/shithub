# Security Dependencies

SP25 adds a local dependency inventory and advisory surface for organization
security overview. The owning code lives in `internal/repos/dependencies`,
`internal/repos/dependencyupdates`,
`internal/worker/jobs/repo_dependency_scan.go`,
`internal/worker/jobs/pr_dependency_review.go`,
`internal/worker/jobs/repo_dependency_update_config_sync.go`, and the web
handlers at `internal/web/handlers/orgs/security.go` and
`internal/web/handlers/repo/pulls.go`.

## Scope

- `repo:dependency_scan` parses supported manifests from the repository default
  branch and stores one current snapshot per repository.
- Supported manifests today are `go.mod`, `package.json`, and
  `package-lock.json`.
- Advisory matching is local-only. Workers do not call GitHub, npm, Go proxy, or
  external vulnerability services on the push path.
- The baseline matcher compares ecosystem, package name, and an exact
  `affected_range` match. Advisories with `affected_range = ''` or `'*'` match
  all versions. Rich semver range evaluation is intentionally not claimed yet.
- The organization security overview is a Team org feature. Free organizations
  see an upgrade banner and no alert or package details.

## Data Model

- `repo_dependency_snapshots` stores the default branch, head SHA, and aggregate
  manifest/dependency counts for the latest scan.
- `repo_dependencies` stores de-duplicated dependency rows keyed by repository,
  ecosystem, package name, and manifest path. Removed dependencies are marked
  stale rather than deleted.
- `dependency_advisories` stores the local advisory catalog. Operators or future
  importers upsert advisories by `(source, external_id)`.
- `repo_dependency_alerts` joins current dependencies to local advisories and
  tracks open, dismissed, and resolved alert state.
- `pull_dependency_reviews` stores a durable pull-request review result keyed
  by PR/base/head SHA.
- `pull_dependency_review_items` stores package-level dependency changes and
  optional local advisory metadata for the PR review surface.
- `dependency_update_configs` stores parsed repository update policy from
  `.github/dependabot.yml` for supported ecosystems.
- `dependency_update_jobs` stores bounded scheduler/update/triage job state.
- `dependency_update_prs` records dependency update pull requests by branch and
  package set.
- `dependency_auto_triage_rules` stores org- or repo-scoped alert rules.
- `dependency_auto_triage_events` stores an audit trail of rule applications.
- `repo_security_advisories` is the repository-maintained advisory table used by
  the org overview. Creation/editing flows are planned separately.

All repository-owned rows use `ON DELETE CASCADE`; advisory creator references
use `ON DELETE SET NULL`.

## Refresh Flow

`push:process` enqueues `repo:dependency_scan` when a repository's default
branch advances. The worker:

1. resolves the default-branch head;
2. reads supported manifests with a 1 MiB per-file cap;
3. upserts the snapshot and dependency rows;
4. marks dependencies stale when they no longer appear at the current head;
5. opens, reopens, or resolves dependency alerts against the local advisory
   catalog.

Existing repositories can be reconciled after deploy with:

```sh
shithubd repo-dependencies-backfill-all
```

The command enqueues one `repo:dependency_scan` job per active repository and
returns immediately.

## Pull Request Dependency Review

`pr:dependency_review` runs when a pull request is opened or synchronized. The
worker:

1. resolves the PR base and head repositories;
2. gates execution behind the Team `dependency_review` entitlement for
   organization-owned repositories;
3. parses supported manifests at the base and head SHAs without mutating repo
   state;
4. stores the dependency diff and local advisory matches; and
5. publishes a completed check run named `Dependency review`.

The check concludes `success` when changed dependencies have no local advisory
matches, `failure` when added or changed dependencies match the advisory
catalog, `neutral` when no supported dependency changes exist, and
`action_required` for Free organizations. Branch protection can require the
`Dependency review` check by name.

The pull request Conversation and Checks tabs render the review result. Team
orgs see package, manifest, version delta, severity, advisory summary, and
recommendation text. Free organizations only see the upgrade-required check
state; private package and advisory details are not persisted or queried for
that path.

## Dependency Update Automation

SP25b starts the dependency update automation foundation. The parser accepts a
GitHub-compatible `.github/dependabot.yml` subset for `gomod` and `npm` entries,
normalizes them to shithub ecosystems `go` and `npm`, and stores schedules,
open PR limits, allow/ignore rules, group rules, registries, unsupported-key
warnings, and a raw config hash. Supported schedule intervals are `daily`,
`weekly`, `monthly`, `quarterly`, `semiannually`, `yearly`, and `cron` with a
`cronjob` value.

Unsupported ecosystems or invalid required fields are diagnostic errors.
Unsupported keys are diagnostic warnings and are not silently treated as active
behavior. This is important for billing parity: Team-plan copy can reference
the feature only once the scheduler and update PR workers are connected, and UI
surfaces must show unsupported configuration rather than making it appear to
work.

The storage layer is intentionally wider than the first worker implementation:
it includes update job state, update PR bookkeeping, and auto-triage rule/audit
tables so follow-up slices can add the scheduler, Go/npm update adapters,
branch creation, grouping, and triage application without reshaping the billing
contract again.

`push:process` enqueues `repo:dependency_update_config_sync` when the default
branch advances. The sync worker:

1. short-circuits personal repositories because the current Team gates apply to
   organization-owned repositories only;
2. checks the owning organization's `dependabot_version_updates` Team
   entitlement before reading or storing config;
3. disables previously parsed configs when the entitlement is denied, the
   default branch is missing, or `.github/dependabot.yml` is absent;
4. validates file type and size before reading the config blob;
5. stores supported config entries and disables stale entries in one
   transaction; and
6. writes a `dependency_update_jobs` `config_sync` record with diagnostics and
   status for operator inspection.

The sync worker still does not create update branches or PRs. Follow-up SP25b
slices must add the bounded scheduler and update workers before the plan
comparison should claim live automated update pull requests.

## Privacy and Product Copy

Organization security overview is only visible to organization members and site
admins. Anonymous users and non-members receive a 404. Free organizations do not
execute the alert-detail queries, so private package names and advisory
summaries are not exposed behind the paywall. Free organization pull requests do
not execute or persist dependency review item details either.

Do not advertise unsupported ecosystems, semver advisory ranges, advisory
import automation, or AI remediation until those subsystems exist. Until the
SP25b workers are connected end to end, user-facing copy should say "supported
Go and npm manifests", "local advisory matches", or "dependency update
configuration" rather than claiming live automated update pull requests.
