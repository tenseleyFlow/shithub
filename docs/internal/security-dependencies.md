# Security Dependencies

SP25 adds a local dependency inventory and advisory surface for organization
security overview. The owning code lives in `internal/repos/dependencies`,
`internal/repos/sbom`, `internal/repos/dependencyupdates`,
`internal/worker/jobs/repo_dependency_scan.go`,
`internal/worker/jobs/pr_dependency_review.go`,
`internal/worker/jobs/repo_dependency_update_config_sync.go`, and the update
workers at `internal/worker/jobs/repo_dependency_update_sweep.go` and
`internal/worker/jobs/repo_dependency_update_run.go`. Advisory import code
lives in `internal/repos/advisoryimport`,
`internal/worker/jobs/dependency_advisory_osv_import.go`, and
`cmd/shithubd/dependency_advisory_import.go`. Web surfaces live in the handlers
at `internal/web/handlers/orgs/security.go`,
`internal/web/handlers/repo/security_sbom.go`, and
`internal/web/handlers/repo/pulls.go`, with repository security advisory
workflows in `internal/web/handlers/repo/security_advisories.go`.

## Scope

- `repo:dependency_scan` parses supported manifests from the repository default
  branch and stores one current snapshot per repository.
- Supported manifests today are `go.mod`, `package.json`, `package-lock.json`,
  `Cargo.toml`, and `Cargo.lock`.
- Advisory matching is local-only. Workers do not call GitHub, npm, crates.io,
  Go proxy, or external vulnerability services on the push or pull-request path.
- Operators can import reviewed OSV JSON files into the local advisory catalog
  through `dependency_advisory:osv_import`; the importer reads local files only.
- Advisory matching compares ecosystem, package name, and the local advisory
  affected ranges through `internal/repos/advisorymatch`. Supported ecosystems
  for range evaluation are Go modules, npm, and Rust crates. Advisories with
  empty or `*` ranges match all versions, and unsupported ecosystems retain
  exact-version matching only.
- Supported range syntax includes exact versions, comparison ranges such as
  `>= v1.0.0, < v1.2.4`, whitespace-separated comparator sets,
  hyphen ranges, npm caret/tilde ranges, wildcard ranges such as `1.2.x`, and
  `||` alternatives. Non-resolved manifest specs such as `^1.2.3` are not
  treated as concrete installed versions for vulnerability matching.
- The organization security overview is a Team org feature. Free organizations
  see an upgrade banner and no alert or package details.

## Data Model

- `repo_dependency_snapshots` stores the default branch, head SHA, and aggregate
  manifest/dependency counts for the latest scan.
- `repo_dependencies` stores de-duplicated dependency rows keyed by repository,
  ecosystem, package name, and manifest path. Removed dependencies are marked
  stale rather than deleted.
- `dependency_advisories` stores the local advisory catalog. Operators or
  importers upsert advisories by `(source, external_id)`. SP25d metadata tracks
  source URLs, modified timestamps, CVSS score/vector, and CWE IDs when an
  imported source provides them.
- `dependency_advisory_sources` stores operator-controlled source configuration
  and licensing/attribution notes. Runtime handlers do not read from remote
  sources.
- `dependency_advisory_aliases` stores GHSA/CVE/OSV aliases for catalog rows.
- `dependency_advisory_affected_ranges` stores normalized imported affected
  package/range details. Alert and pull-request review queries prefer these
  structured rows and fall back to the legacy single `affected_range` column
  only when an advisory has no structured ranges.
- `dependency_advisory_sync_runs` records import audit history, counts, and
  failures without logging private repository package data.
- `repo_dependency_alerts` joins current dependencies to local advisories and
  tracks open, dismissed, and resolved alert state. Alert refresh is reconciled
  in Go so ecosystem-aware range matching, stale dependency resolution, and
  withdrawn advisory resolution share one matcher.
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
  repo advisory pages and the org overview. It stores draft/published/withdrawn
  /archived state, severity, affected package metadata, optional GHSA/CVE IDs,
  reference URLs, and lifecycle timestamps.
- `repo_security_advisory_events` stores the advisory timeline for create,
  update, publish, withdraw, archive, and reopen actions.
- `repo_security_advisory_collaborators` stores per-advisory user/team
  disclosure collaborators and their recorded roles.
- `repo_sbom_exports` stores the latest generated repository SBOM per format.
  The v1 format is SPDX 2.3 JSON derived from the latest stored dependency
  snapshot.

All repository-owned rows use `ON DELETE CASCADE`; advisory creator references
use `ON DELETE SET NULL`.

## Repository Security Advisories

Repositories expose a GitHub-shaped advisory surface at
`/{owner}/{repo}/security/advisories`. Maintainers can create a draft advisory,
edit package/severity/description/reference metadata, publish it, withdraw it,
archive it, or reopen it as a draft. Advisory descriptions render through the
canonical `internal/markdown` sanitizer before reaching templates.

Advisory managers can add user or organization-team collaborators to an
advisory. Collaborators can view draft, withdrawn, and archived advisory
details through the advisory URL, including for private repositories where they
do not otherwise have repository read access. Collaborator roles are recorded
as `read`, `write`, or `admin`; SP25c uses them for disclosure membership only,
while advisory edits, state changes, and collaborator management remain gated
by repository settings policy.

When a repository advisory has affected ecosystem and package metadata, shithub
mirrors it into the local `dependency_advisories` catalog under a repo-scoped
source key. Published advisories have `withdrawn_at = NULL` and immediately
refresh matching dependency alerts across current repository dependency rows.
Draft, withdrawn, archived, and reopened advisories are kept withdrawn in the
catalog so existing dependency-review and dependency-scan queries ignore them,
and open alerts from a previously published advisory are resolved.

Published advisories are visible to normal repository readers. Draft,
withdrawn, and archived advisories are visible to viewers who can manage
general repository settings or who were added as advisory collaborators.
Advisory writes are always policy-gated through
`policy.ActionRepoSettingsGeneral`.

Public repositories and personal repositories can use the baseline advisory
workflow. Private organization repositories require the Team
`security_advisories` entitlement for create, edit, and state transitions. When
the entitlement is denied, the UI renders an upgrade path and does not create
or mutate advisory rows. Existing advisory details remain readable according to
the state visibility rules above so downgrade handling does not hide already
published security information from repository readers.

## Repository SBOM Exports

Repositories expose SPDX JSON SBOM exports at
`/{owner}/{repo}/security/sbom`. The page reads the latest
`repo_dependency_snapshots` and current `repo_dependencies` rows; it does not
execute package managers or contact remote registries on the request path.

Authenticated repository readers can generate an export when a dependency
snapshot exists. Downloads serve the latest stored `spdx-json` document from
`repo_sbom_exports`, so a generated file remains stable until someone
regenerates it. If a newer dependency snapshot exists, the page marks the
stored export stale rather than silently changing the downloaded artifact.

Public repositories and personal repositories can generate and download SBOMs
at baseline. Private organization repositories require the Team `sboms`
entitlement before the page loads dependency/package details, generates a new
export, or serves the stored document.

## Refresh Flow

`push:process` enqueues `repo:dependency_scan` when a repository's default
branch advances. The worker:

1. resolves the default-branch head;
2. reads supported manifests with a 1 MiB per-file cap;
3. upserts the snapshot and dependency rows;
4. marks dependencies stale when they no longer appear at the current head;
5. opens, reopens, or resolves dependency alerts against the local advisory
   catalog with Go/npm/Rust range matching; and
6. for Team organization repositories with enabled dependency update configs,
   enqueues bounded security-update jobs when matching open alerts exist and no
   security update PR/job is already active.

Existing repositories can be reconciled after deploy with:

```sh
shithubd repo-dependencies-backfill-all
```

The command enqueues one `repo:dependency_scan` job per active repository and
returns immediately.

## Advisory Imports

Operators can import OSV-formatted advisory data from a reviewed local file:

```sh
shithubd dependency-advisories-import-osv \
  --file /srv/shithub/advisories/osv-go-npm-rust.json \
  --source-name osv \
  --source-url https://osv.dev \
  --license CC-BY-4.0 \
  --attribution "Open Source Vulnerabilities"
```

The command validates the local file path and enqueues
`dependency_advisory:osv_import`; the worker reads the file, records a
`dependency_advisory_sync_runs` row, and transactionally upserts
`dependency_advisories`, aliases, CVSS/CWE metadata, and structured affected
ranges. The importer accepts one OSV object or an array of OSV objects. Current
normalization stores Go module, npm, and crates.io affected packages;
unsupported ecosystems are skipped rather than treated as matched.

Import failures update both the sync run and source row with a bounded error
message. Retry by fixing the operator-controlled file and enqueueing the
command again. After importing new advisories, run
`shithubd repo-dependencies-backfill-all` if existing repository inventories
need alert recomputation. Push and pull-request paths continue to read only the
local catalog.

## Pull Request Dependency Review

`pr:dependency_review` runs when a pull request is opened or synchronized. The
worker:

1. resolves the PR base and head repositories;
2. gates execution behind the Team `dependency_review` entitlement for
   organization-owned repositories;
3. parses supported manifests at the base and head SHAs without mutating repo
   state;
4. stores the dependency diff and local advisory matches using the same
   Go/npm/Rust range matcher as repository alerts; and
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
application, repo/org settings surfaces for update diagnostics, live/network
advisory sync, additional advisory source formats, richer lockfile adapters,
and a dedicated bot identity for automated commits. Do not describe those as
shipped behavior.

## Privacy and Product Copy

Organization security overview is only visible to organization members and site
admins. Anonymous users and non-members receive a 404. Free organizations do not
execute the alert-detail queries, so private package names and advisory
summaries are not exposed behind the paywall. Free organization pull requests do
not execute or persist dependency review item details either.

Do not advertise unsupported ecosystems, live advisory sync automation,
auto-triage application, lockfile-only remediation, or AI remediation until
those subsystems exist. User-facing copy can mention operator-controlled local
OSV imports when the audience is administrative. Product copy should say
"supported Go, npm, and Rust manifests", "local advisory matches",
"Go/npm/Rust advisory ranges", or "dependency update pull requests for
supported direct Go and npm dependencies" rather than claiming complete
package-manager parity.
