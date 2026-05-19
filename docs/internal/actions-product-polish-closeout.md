# Actions Product Polish Closeout

This is the S41l closeout packet. S41l followed the S41k Actions UI parity
foundation and closed the highest-value daily-use gaps without changing the
runner trust boundary.

For the broader UI backlog, see
[actions-ui-parity-closeout.md](./actions-ui-parity-closeout.md). For shared
runner safety and rollout gates, see
[actions-public-runners.md](./actions-public-runners.md). For the full CI
dogfood decision, see [actions-ga-readiness.md](./actions-ga-readiness.md).

## Verdict

Status: **S41l product-polish scope complete; do not call this full GitHub
Actions parity.**

The Actions surface now supports the ordinary workflow a GitHub Actions user
expects for simple CI:

1. Create a supported workflow from the repository Actions UI.
2. Pin important workflows in the sidebar.
3. Read completed logs with grouped sections and stable line links.
4. See Checks-backed status on PR list/detail/merge surfaces and jump back to
   local shithub Actions run pages.

The remaining gaps are not S41l-sized UI fixes. They belong to runtime/cache,
runner-management, billing/metrics, supply-chain, marketplace/toolchain, and
API-compatibility tracks.

## S41l Slices

| Slice | PR | Status | Shipped behavior |
| --- | --- | --- | --- |
| S41l-1 workflow authoring | #284 | Done | `New workflow` picker, supported starter templates, editor path, workflow parser validation, and honest unsupported-template copy. |
| S41l-2 workflow pinning | #294 | Done | Viewer-scoped workflow pins, deterministic pinned ordering, sidebar pin/unpin/move controls, and stale-file-safe rendering. |
| S41l-3 log groups/permalinks | #298 | Done | Completed-log `::group::` rendering, escaped group titles, stable `L<N>` anchors, copyable line/range links, and unchanged raw downloads. |
| S41l-4 PR/checks parity | #300 | Done | PR list/detail check rollups, required-check copy in merge context, safe local Actions links, and writer-gated rerun forms. |
| S41l-5 closeout | current | In review | Durable closeout docs, audit evidence map, React decision, and follow-up ownership. |

## Current Product Contract

Workflow authoring:

- Repository writers can create `.shithub/workflows/*.yml` files from supported
  starter templates.
- The web flow validates path, branch, file collision, and workflow syntax
  before committing.
- GitHub-style templates that need unsupported marketplace/setup/cache/matrix
  features are shown as unavailable rather than offered as broken runs.

Workflow pinning:

- Pins are viewer-scoped, repository-scoped preferences.
- Pins do not reveal workflows a viewer cannot otherwise see.
- Missing workflow files are ignored during sidebar rendering so stale pins do
  not create broken links for ordinary readers.

Step logs:

- Completed logs render `::group::<title>` / `::endgroup::` markers as grouped
  UI sections.
- Log text and group titles remain text, not HTML.
- Line anchors are deterministic physical-line IDs like `L1`.
- Raw log downloads remain byte-for-byte the same as persisted logs.
- Live SSE logs still render the raw streaming path; completed-log grouping is
  a display parser, not an ingestion format.

PR/check surfaces:

- PR list rows, PR headers, merge context, and Checks tab use the same
  Checks-backed rollups and required-check evaluator as mergeability.
- Local same-repo Actions `details_url` values link to shithub run pages.
- Rerun controls are visible only to viewers with repo write permission and
  post through CSRF-protected forms.
- External or cross-repo `details_url` values remain API data but are not
  rendered as browser links in PR templates.

## Evidence

Focused implementation evidence:

- Workflow authoring: `internal/actions/workflowtemplates`,
  `internal/web/handlers/repo/actions_new_workflow.go`, and repo handler tests
  covering auth, path validation, parser diagnostics, and successful commits.
- Workflow pinning: `user_action_workflow_pins`, Actions sqlc pin queries,
  `internal/web/handlers/repo/actions_pins.go`, and tests covering viewer
  scope, stale files, ordering, and CSRF-protected controls.
- Log groups and links: `internal/actions/logview`, completed step-log template
  rendering, unchanged download route, and tests covering escaping, malformed
  groups, anchors, and raw downloads.
- PR/check parity: `internal/web/handlers/repo/check_links.go`,
  `pull_checks_parity_test.go`, `code_checks*_test.go`, and
  `docs/internal/checks.md`.

Run this local packet when auditing S41l:

```sh
SHITHUB_TEST_DATABASE_URL=... go test -trimpath ./internal/web/handlers/repo -run 'TestActionsProduction|TestRepoActionsWorkflowPin|TestPull|TestCodeCheck|TestLocalActionsRun'
go test -trimpath ./internal/actions/logview ./internal/checks/...
make build
```

Run the browser screenshot harness when a deterministic target repo exists:

```sh
SHITHUB_URL=https://shithub.sh \
SHITHUB_ACTIONS_REPO=mfwolffe/scratch \
SHITHUB_ACTIONS_WORKFLOW=smoke.yml \
SHITHUB_ACTIONS_RUN=<known-run-index> \
SHITHUB_ACTIONS_AUDIT_ROUTES='new=/mfwolffe/scratch/actions/new' \
make audit-actions-ui
```

The screenshot harness requires `puppeteer`; do not mark that check complete
unless it produced a manifest under `.refs/actions-ui-audit/`.

## Findings

No Critical or High UI security findings are open from S41l.

Medium/known limitations:

- Live SSE log pages do not fold groups while a step is still streaming.
  Completed logs render grouped sections after chunks are finalized or read
  from object storage.
- The Actions screenshot harness remains manual/operator-run. It is not a CI
  visual regression system until a deterministic seed repo and browser baseline
  exist.
- Workflow templates cover supported shithub v1 CI patterns only. Marketplace,
  setup/cache actions, matrix, services, reusable workflows, OIDC, and hosted
  image provisioning remain runtime/toolchain gaps.
- The management pages for caches, runners, usage metrics, performance metrics,
  and attestations are still first-pass or honest empty-state surfaces.

## React Decision

Keep the current server-rendered template model.

S41l added more interaction, but not the kind that needs a shared client
runtime. The new controls are small and page-scoped: form-backed workflow
creation, form-backed sidebar pins, log copy/toggle helpers, and PR/check
links. A React island would add a build pipeline, CSP review, dependency
patching, hydration failure modes, and a second test surface without removing
meaningful complexity today.

Reconsider React only when the same stateful component needs to be reused
across multiple Actions pages, or when the graph/log surfaces outgrow the
current vanilla islands.

## Follow-Up Map

| Gap | Owner track | Notes |
| --- | --- | --- |
| Runtime recovery and pool operations | S41m | Shipped after the 2026-05-17 stale-runner incident: active-set reconciliation, SQL-free recovery commands, three-runner static pool, and pool SSH/firewall documentation are in production. Remaining ops caveat is managed Grafana alert provisioning for committed Prometheus rules. |
| Cache management and cache action protocol | Actions runtime + UI follow-up | Start with list/delete/filter/quota UX; implement protocol compatibility only with a runtime contract and quota accounting. |
| Runner management UI | S41j/S41k follow-up after policy design | Needs repo/org policy, audit logging, one-time token display, revocation flows, and host-detail redaction. |
| Billing-grade metrics | Payments/SP23 + Actions metrics | Period selectors, export, and usage views must read from entitlements/metering, not scattered plan checks. |
| Attestations/provenance | S54 | Requires persistence, upload/publish API, provenance rendering, and supply-chain security review. |
| Marketplace/toolchain parity | S41i + later runtime campaign | First-party setup/cache/toolchain steps, matrix, services, reusable workflows, OIDC, and hosted image contracts belong outside UI polish. |
| Classic Statuses API | S50 or dedicated API sprint | shithub supports Checks; GitHub-compatible commit statuses remain an explicit product/API decision. |
| Visual regression CI | UI QA track | Promote the screenshot harness to CI only after deterministic fixture data and stable browser/fonts exist. |

## Next Recommendation

Close S41l and treat the Actions campaign as ready for owner-track follow-ups
rather than more omnibus S41 UI polish. The next highest-leverage choices are:

1. **Grafana alert provisioning** for committed Actions alert rules if we want
   S41m's idle-with-assigned-jobs detector to page without manual UI setup.
2. **Cache management/protocol work** if the goal is GitHub Actions parity for
   common dependency-cache workflows.
3. **Runner management UI** only after a policy/audit design, because token
   display and host details are security-sensitive.
4. **Marketplace/toolchain parity** if the goal is moving shithub's own larger
   CI workflows from GitHub Actions to shithub Actions.
5. **S42** if the Actions effort should pause now that simple workflows are
   dogfooding successfully on the shared pool.
