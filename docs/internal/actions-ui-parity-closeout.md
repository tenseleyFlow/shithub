# Actions UI Parity Closeout

This is the S41k/S41l closeout packet for the repository Actions product
surface. It summarizes what is now present, what remains intentionally
first-pass, and which follow-up tracks should own the remaining GitHub parity
gaps.

S41k established the Actions UI foundation. S41l closed the highest-leverage
product-polish gaps from that audit: workflow authoring, workflow pinning, log
group/permalink ergonomics, and PR/check affordances. For the detailed S41l
handoff, see
[actions-product-polish-closeout.md](./actions-product-polish-closeout.md).

This UI packet does not replace the public-runner safety gates in
[actions-public-runners.md](./actions-public-runners.md), and it does not solve
the full CI/toolchain migration criteria in
[actions-ga-readiness.md](./actions-ga-readiness.md).

## Current Verdict

Status: **Actions product surface is usable and GitHub-shaped; follow-up
backlog remains before full GitHub parity claims.**

The core Actions product can now be navigated with GitHub-like muscle memory:
run lists, workflow routes, run summaries, graph canvas, logs, annotations,
management pages, Code-surface status indicators, workflow authoring, workflow
pinning, grouped completed logs, line permalinks, and PR/check status
affordances exist. The remaining gaps are no longer "missing Actions tab";
they are specific runtime, management, analytics, and API subfeatures such as
cache protocol compatibility, workflow-published attestations, repo/org runner
registration UI, billing-grade metrics, and classic Statuses API compatibility.

The current implementation remains server-rendered Go templates plus small
page-scoped JavaScript islands. S41l did not create enough shared client-side
state to justify a React island or a frontend build pipeline. Revisit that
decision only if multiple Actions pages begin to share complex client state
that cannot be kept small and testable with the existing approach.

## Present Surfaces

| Surface | Status | Notes |
| --- | --- | --- |
| Actions list | Present | GitHub-shaped sidebar, personal workflow pins, query filter, workflow route links, header dropdown scaffolds, row menus, and run controls. |
| Workflow-specific run list | Present | Safe route normalization under `.shithub/workflows/`; legacy query filters still resolve. |
| Workflow authoring | Present | `New workflow` opens a supported template picker and editor path with parser validation and honest unsupported-template copy. |
| Run summary | Present | Status header, summary strip, sidebar, rerun/cancel controls, annotations, and graph canvas. |
| Graph canvas | Present | Vanilla JS island with pan, zoom, fit, focus, and draggable popover. No React build step. |
| Step log | Present | In-app logs, SSE/live path, shithub-served download route, grouped completed logs, line/range permalinks, and step-local annotations. |
| PR/check surfaces | Present | PR list, PR header, merge context, and Checks tab show check rollups; local Actions details URLs link to run pages and rerun forms are writer-gated. |
| Caches | First pass | Lists real `workflow_caches` rows when present. Full cache protocol/delete/filter UX remains open. |
| Attestations | Placeholder | Honest "No attestations" workflow surface; SP29 adds manual repository in-toto statement storage, but Actions does not yet publish provenance. |
| Runners | First pass | Shows repo-relevant runner inventory without registration tokens or host-sensitive details. |
| Usage metrics | First pass | Bounded current-month repo metrics; not billing-grade analytics. |
| Performance metrics | First pass | Bounded current-month runtime/queue/failure views; not historical drilldown parity. |
| Code-surface status | Present | Tree, commit list/detail, branches, and compare surfaces show check rollups for visible commits. |

## Closed S41k Audit Gaps

| ID | Gap | Type | Resolution |
| --- | --- | --- | --- |
| S41K8-G1 | Real `New workflow` UX: template picker, workflow editor, and unsupported-template copy. | UI + workflow authoring | Closed by S41l-1. |
| S41K8-G2 | Workflow pinning: persisted pin state, sidebar ordering, and pin/unpin controls. | UI preference | Closed by S41l-2. |
| S41K8-G7 | Log viewer polish: `::group::` folding, stable line/chunk permalinks, copied anchors, and keyboard paths. | UI polish | Closed by S41l-3 for completed logs and permalink basics. |
| S41K8-G8 | PR/checks parity: PR list/detail status affordances, check-suite grouping, failed-required-check copy, and rerun links. | Code/PR UI | Closed by S41l-4 for Checks-backed status affordances. |

## Remaining Gaps

| ID | Gap | Type | Recommended owner |
| --- | --- | --- | --- |
| S41K8-G3 | Cache management: filtering, deletion, protocol/API compatibility, action integration, and quota/retention semantics. | Runtime + UI | Actions cache/runtime follow-up + PAYMENTS/SP23 for quota. |
| S41K8-G4 | Attestations: workflow automatic generation, publish API, provenance rendering, signature verification, and security review. | Supply chain | S54. |
| S41K8-G5 | Runner management UI: repo/org registration, revocation, one-time token display, policy gates, and audit logging. | Security-sensitive UI | S41j/S41m follow-up after policy design. |
| S41K8-G6 | Metrics and usage: historical period selectors, export/download, billing alignment, and performance drilldowns. | Analytics + billing | PAYMENTS/SP23 + Actions metrics follow-up. |
| S41K8-G9 | Classic GitHub Statuses API compatibility. | API/product decision | S50 or post-MVP checks sprint. |
| S41K8-G10 | Marketplace/toolchain parity: first-party setup/cache steps, matrix, services, reusable workflows, OIDC, and hosted runner image contract. | Runtime/toolchain | S41i + later Actions runtime campaign. |
| S41L5-G1 | Live-log group rendering parity. | UI polish | Later log viewer polish; completed logs are grouped today, live SSE remains raw text. |
| S41L5-G2 | Deterministic visual regression baseline in CI. | QA infrastructure | Future UI QA track after a seeded Actions fixture exists. |

## Recommended Sequence

1. **Grafana alert provisioning for Actions.** S41m committed the
   idle-with-assigned-jobs alert expression and deployed the metric, but
   production uses Grafana Cloud remote-write rather than a local Prometheus
   that loads `deploy/monitoring/prometheus/rules.yml`.
2. **Cache management.** Do deletion/filtering first; defer full protocol
   compatibility until the runtime cache-action contract is clear.
3. **Metrics/usage.** Align with billing/accounting before claiming GitHub
   usage parity. Avoid scattered plan checks outside entitlements.
4. **Runner management UI.** Only after policy, audit logging, token display,
   and operator boundaries are specified. This surface can leak sensitive
   infrastructure details if rushed.
5. **Attestations and supply chain.** Route workflow-published attestations to
   S54 rather than forcing them into Actions UI parity.
6. **Marketplace/toolchain parity.** Treat as runtime work, not UI polish. It
   is the blocker for moving shithub's own full CI to shithub Actions.
7. **Statuses API compatibility.** Make this an explicit API/product sprint
   rather than a template shortcut.

## Evidence Checklist

S41k-8 local evidence captured on 2026-05-15:

- `make audit-actions-ga`
- `SHITHUB_TEST_DATABASE_URL=... GOCACHE=/private/tmp/shithub-s41k-8/.gocache go test -trimpath ./internal/web/handlers/repo`
- `GOCACHE=/private/tmp/shithub-s41k-8/.gocache go test -trimpath ./internal/checks/... ./internal/actions/annotations ./internal/actions/logstream`
- `GOCACHE=/private/tmp/shithub-s41k-8/.gocache make build`

Post-deploy evidence captured on 2026-05-16:

- A fresh `mfwolffe/scratch` push at
  `6aac17a4c2eca6c5be94a566a571d1dd42c06939` created check run `109` with
  `details_url=/mfwolffe/scratch/actions/runs/5`; that run route returned HTTP
  200.
- `espadonne/scratch` private-route masking was manually verified in
  production: anonymous HTTP requests to the repo root and Actions route both
  returned 404; SSH with the `mfwolffe` key returned `repository not found`;
  SSH with the `espadonne` key successfully listed `refs/heads/trunk`.
- The shared `ubuntu-latest` runner was reachable, but there was an existing
  queued backlog at the time of the check. The `details_url` invariant is
  proven at check-row creation time; run completion was not part of this UI
  closeout check.

S41l local evidence captured in the S41l implementation slices:

- PR #284 added workflow authoring tests for authorization, path validation,
  parser diagnostics, and successful template commits.
- PR #294 added workflow pin tests for viewer scope, stale workflow files,
  deterministic ordering, and CSRF-protected pin controls.
- PR #298 added completed-log parser/render tests for escaping, nested and
  unclosed groups, line anchors, range links, and unchanged raw downloads.
- PR #300 added PR/check parity tests for rollup states, required-check copy,
  private-repo masking, safe local Actions links, and writer-gated rerun forms.

Run the current S41l audit packet with:

```sh
SHITHUB_TEST_DATABASE_URL=... go test -trimpath ./internal/web/handlers/repo -run 'TestActionsProduction|TestRepoActionsWorkflowPin|TestPull|TestCodeCheck|TestLocalActionsRun'
go test -trimpath ./internal/actions/logview ./internal/checks/...
make build
```

## React Decision

Current decision: **no React through S41l by default.**

Reasons:

- server-rendered templates preserve the existing security and deployment
  model;
- the graph island is contained in one vanilla JS file;
- S41l's new interactions are still small, page-local controls: workflow
  picker/editor forms, sidebar pin POSTs, log-group toggles/copy links, and
  PR/check links;
- no frontend dependency patch pipeline or hydration failure mode is required;
- existing template tests and screenshot harness cover the important landmarks.

Reconsider React only if future work introduces repeated, stateful controls
across multiple Actions pages that cannot be kept maintainable as small
page-local islands.

## Non-Goals For Actions UI Follow-ups

- Do not implement marketplace action compatibility under a UI sprint.
- Do not expose runner registration tokens until the policy/audit model is
  specified.
- Do not call metrics "usage billing parity" until entitlements and metering
  are the source of truth.
- Do not treat the Actions Attestations empty state as workflow provenance
  support.
- Do not implement classic Statuses API compatibility accidentally through
  template shortcuts; make it an explicit API/product decision.
