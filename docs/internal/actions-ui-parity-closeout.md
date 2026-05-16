# Actions UI Parity Closeout

This is the S41k closeout packet for the repository Actions product surface.
It summarizes what is now present, what remains intentionally first-pass, and
which follow-up tracks should own the remaining GitHub parity gaps.

S41k is UI/product parity. It does not replace the public-runner safety gates
in [actions-public-runners.md](./actions-public-runners.md), and it does not
solve the full CI/toolchain migration criteria in
[actions-ga-readiness.md](./actions-ga-readiness.md).

## Current Verdict

Status: **UI parity foundation complete; follow-up backlog required for full
GitHub parity claims.**

The core Actions product can now be navigated with GitHub-like muscle memory:
run lists, workflow routes, run summaries, graph canvas, logs, annotations,
management pages, and Code-surface status indicators exist. The remaining gaps
are no longer "missing Actions tab"; they are specific product subfeatures such
as workflow authoring, pinning, cache protocol compatibility, attestations,
runner registration UI, richer metrics, log folding, and broader status/check
compatibility.

The current implementation remains server-rendered Go templates plus small
page-scoped JavaScript islands. Do not introduce React by default for the
current surface. Revisit that decision only if multiple Actions pages begin to
share complex client-side state that cannot be kept small and testable with the
existing approach.

## Present Surfaces

| Surface | Status | Notes |
| --- | --- | --- |
| Actions list | Present | GitHub-shaped sidebar, query filter, workflow route links, header dropdown scaffolds, row menus, and run controls. |
| Workflow-specific run list | Present | Safe route normalization under `.shithub/workflows/`; legacy query filters still resolve. |
| Run summary | Present | Status header, summary strip, sidebar, rerun/cancel controls, annotations, and graph canvas. |
| Graph canvas | Present | Vanilla JS island with pan, zoom, fit, focus, and draggable popover. No React build step. |
| Step log | Present | In-app logs, SSE/live path, shithub-served download route, and step-local annotations. |
| Caches | First pass | Lists real `workflow_caches` rows when present. Full cache protocol/delete/filter UX remains open. |
| Attestations | Placeholder | Honest "No attestations" surface; no provenance model exists yet. |
| Runners | First pass | Shows repo-relevant runner inventory without registration tokens or host-sensitive details. |
| Usage metrics | First pass | Bounded current-month repo metrics; not billing-grade analytics. |
| Performance metrics | First pass | Bounded current-month runtime/queue/failure views; not historical drilldown parity. |
| Code-surface status | Present | Tree, commit list/detail, branches, and compare surfaces show check rollups for visible commits. |

## Open Gaps

| ID | Gap | Type | Recommended owner |
| --- | --- | --- | --- |
| S41K8-G1 | Real `New workflow` UX: template picker, workflow editor, and unsupported-template copy. | UI + workflow authoring | S41k follow-up |
| S41K8-G2 | Workflow pinning: persisted pin state, sidebar ordering, and pin/unpin controls. | UI preference | S41k follow-up |
| S41K8-G3 | Cache management: filtering, deletion, protocol/API compatibility, action integration, and quota/retention semantics. | Runtime + UI | S41k follow-up + PAYMENTS/SP23 for quota |
| S41K8-G4 | Attestations: persistence model, upload/publish API, provenance rendering, and security review. | Supply chain | S54 |
| S41K8-G5 | Runner management UI: repo/org registration, revocation, one-time token display, policy gates, and audit logging. | Security-sensitive UI | S41j follow-up or S41k follow-up after policy design |
| S41K8-G6 | Metrics and usage: historical period selectors, export/download, billing alignment, and performance drilldowns. | Analytics + billing | PAYMENTS/SP23 + S41k follow-up |
| S41K8-G7 | Log viewer polish: `::group::` folding, stable line/chunk permalinks, copied anchors, and keyboard paths. | UI polish | S41k follow-up |
| S41K8-G8 | PR/checks parity: PR list/detail status affordances, check-suite grouping, failed-required-check copy, and rerun links. | Code/PR UI | S41k follow-up, with checks domain review |
| S41K8-G9 | Classic GitHub Statuses API compatibility. | API/product decision | S50 or post-MVP checks sprint |
| S41K8-G10 | Marketplace/toolchain parity: first-party setup/cache steps, matrix, services, reusable workflows, OIDC, and hosted runner image contract. | Runtime/toolchain | S41i + later Actions runtime campaign |

## Recommended Sequence

1. **S41k closeout audit after S41k-7 deploy.** Run the screenshot harness
   against production, capture Code-surface status evidence, and verify no
   Critical/High UI security findings.
2. **Workflow authoring and pinning.** These are the highest-leverage pure UI
   gaps and make the Actions section feel less placeholder-driven.
3. **Log viewer polish.** Group folding and permalinks are visible on every
   non-trivial workflow and remain contained enough for one reviewable PR.
4. **PR/checks parity.** Now that Code surfaces show status indicators, audit
   PR list/detail/checks paths against GitHub and close cross-link gaps.
5. **Cache management.** Do deletion/filtering first; defer full protocol
   compatibility until the runtime cache-action contract is clear.
6. **Metrics/usage.** Align with billing/accounting before claiming GitHub
   usage parity. Avoid scattered plan checks outside entitlements.
7. **Runner management UI.** Only after policy, audit logging, token display,
   and operator boundaries are specified. This surface can leak sensitive
   infrastructure details if rushed.
8. **Attestations and supply chain.** Route to S54 rather than forcing it into
   Actions UI parity.
9. **Marketplace/toolchain parity.** Treat as runtime work, not UI polish. It
   is the blocker for moving shithub's own full CI to shithub Actions.

## Evidence Checklist

S41k-8 local evidence captured on 2026-05-15:

- `make audit-actions-ga`
- `SHITHUB_TEST_DATABASE_URL=... GOCACHE=/private/tmp/shithub-s41k-8/.gocache go test -trimpath ./internal/web/handlers/repo`
- `GOCACHE=/private/tmp/shithub-s41k-8/.gocache go test -trimpath ./internal/checks/... ./internal/actions/annotations ./internal/actions/logstream`
- `GOCACHE=/private/tmp/shithub-s41k-8/.gocache make build`

Local checks:

```sh
make audit-actions-ga
SHITHUB_TEST_DATABASE_URL=... go test -trimpath ./internal/web/handlers/repo
go test -trimpath ./internal/checks/... ./internal/actions/annotations ./internal/actions/logstream
make build
```

Manual production checks after S41k-8 deploy:

1. Run `make audit-actions-ui` against `https://shithub.sh` and a known
   `mfwolffe/scratch` run.
2. Confirm the manifest has `200` responses for list, workflow, run summary,
   step log, caches, attestations, runners, usage metrics, and performance
   metrics.
3. Confirm no non-graph horizontal overflow in desktop/mobile captures.
4. Confirm the Code tab for a recent green run shows a green check in the repo
   heading and that commit list/detail, branches, and compare links resolve to
   local shithub Actions pages.
5. Confirm step log downloads stay on shithub routes and never expose raw
   object-storage URLs in the page source.
6. Confirm private repository Actions routes and Code-surface status affordances
   preserve existence-leak-safe 404 behavior for unauthorized viewers.

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

## React Decision

Current decision: **no React in S41k by default.**

Reasons:

- server-rendered templates preserve the existing security and deployment
  model;
- the graph island is contained in one vanilla JS file;
- no frontend dependency patch pipeline or hydration failure mode is required;
- existing template tests and screenshot harness cover the important landmarks.

Reconsider React only if future work introduces repeated, stateful controls
across multiple Actions pages that cannot be kept maintainable as small
page-local islands.

## Non-Goals For S41k Follow-ups

- Do not implement marketplace action compatibility under a UI sprint.
- Do not expose runner registration tokens until the policy/audit model is
  specified.
- Do not call metrics "usage billing parity" until entitlements and metering
  are the source of truth.
- Do not treat the Attestations empty state as provenance support.
- Do not implement classic Statuses API compatibility accidentally through
  template shortcuts; make it an explicit API/product decision.
