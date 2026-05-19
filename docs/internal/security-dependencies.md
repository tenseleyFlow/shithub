# Security Dependencies

SP25 adds a local dependency inventory and advisory surface for organization
security overview. The owning code lives in `internal/repos/dependencies`,
`internal/repos/dependencyupdates`,
`internal/worker/jobs/repo_dependency_scan.go`,
`internal/worker/jobs/pr_dependency_review.go`,
`internal/worker/jobs/repo_dependency_update_config_sync.go`, and the update
workers at `internal/worker/jobs/repo_dependency_update_sweep.go` and
`internal/worker/jobs/repo_dependency_update_run.go`. Web surfaces live in the
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
   catalog; and
6. for Team organization repositories with enabled dependency update configs,
   enqueues bounded security-update jobs when matching open alerts exist and no
   security update PR/job is already active.

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
warnings, the next due check time, and a raw config hash. Supported schedule
intervals are `daily`, `weekly`, `monthly`, `quarterly`, `semiannually`,
`yearly`, and `cron` with a `cronjob` value. `daily` schedules run on weekdays,
`weekly` defaults to Monday, monthly and longer intervals run on the first day
of their cadence month, and cron uses standard five-field crontab syntax in UTC.
When a config omits `schedule.time`, shithub assigns a deterministic per-entry
minute of day from the config hash, ecosystem, and directory to avoid a
thundering herd while keeping repeated syncs stable.

Unsupported ecosystems or invalid required fields are diagnostic errors.
Unsupported keys are diagnostic warnings and are not silently treated as active
behavior. This is important for billing parity: Team-plan copy should reference
only the supported ecosystem/configuration envelope, and UI surfaces must show
unsupported configuration rather than making it appear to work.

`push:process` enqueues `repo:dependency_update_config_sync` when the default
branch advances. The sync worker:

1. short-circuits personal repositories because the current Team gates apply to
   organization-owned repositories only;
2. checks the owning organization's `dependabot_version_updates` Team
   entitlement before reading or storing config;
3. disables previously parsed configs when the entitlement is denied, the
   default branch is missing, or `.github/dependabot.yml` is absent;
4. validates file type and size before reading the config blob;
5. stores supported config entries with computed `next_run_at` timestamps and
   disables stale entries in one transaction; and
6. writes a `dependency_update_jobs` `config_sync` record with diagnostics and
   status for operator inspection.

`repo:dependency_update_sweep` is the periodic scheduler worker. Operators
should enqueue it on the same kind of timer beat used for other sweep workers.
The sweep claims due `dependency_update_configs` with row locks, creates
`dependency_update_jobs` rows with `job_kind = 'version_update'`, advances each
config's `next_run_at`, enqueues `repo:dependency_update_run`, and
self-enqueues when it fills its batch.

`repo:dependency_scan` also creates queued `security_update` jobs after alert
refresh when all of the following are true:

- the repository is organization-owned;
- the organization has the Team `dependabot_security_updates` entitlement;
- an enabled dependency update config matches at least one open alert; and
- there is no active queued/running security update job and no open security
  dependency update PR for the repository.

`repo:dependency_update_run` idempotently claims only queued
`dependency_update_jobs`, rechecks repository and entitlement gates, builds the
supported dependency snapshot, plans candidate updates, creates
`shithub/dependency-updates/...` branches, commits manifest changes through the
same `webedit` path as browser edits, opens pull requests with the existing PR
service, and records `dependency_update_prs` bookkeeping. Version update jobs
resolve latest versions for direct Go and npm dependencies. Security update
jobs use local open advisory alerts and require an exact patched version from
the local advisory catalog. Grouped update rules can combine related Go/npm
direct manifest updates into one pull request. Open PR limits are enforced
before branches are created.

The current adapters deliberately avoid running arbitrary package-manager
scripts in the server process. Go version resolution shells out to
`go list -m -json -versions` with a bounded timeout; npm version resolution
performs a bounded registry metadata HTTP request and reads `dist-tags.latest`.
Manifest edits are limited to supported direct dependencies in `go.mod` and
`package.json`. Lockfile-only and transitive updates are detected but skipped
until a package-manager adapter can update them safely.

Still-planned SP25b/SP25 follow-up work includes auto-triage rule UI and worker
application, repo/org settings surfaces for update diagnostics, richer semver
range evaluation, richer lockfile adapters, and a dedicated bot identity for
automated commits. Do not describe those as shipped behavior.

## Privacy and Product Copy

Organization security overview is only visible to organization members and site
admins. Anonymous users and non-members receive a 404. Free organizations do not
execute the alert-detail queries, so private package names and advisory
summaries are not exposed behind the paywall. Free organization pull requests do
not execute or persist dependency review item details either.

Do not advertise unsupported ecosystems, semver advisory ranges, advisory
import automation, auto-triage application, lockfile-only remediation, or AI
remediation until those subsystems exist. User-facing copy should say
"supported Go and npm manifests", "local advisory matches", or "dependency
update pull requests for supported direct dependencies" rather than claiming
complete package-manager parity.
