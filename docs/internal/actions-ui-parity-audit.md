# Actions UI Parity Audit

This document records the S41k/S41l audit harness for the repository Actions
UI. S41k established the Actions UI foundation; S41l added workflow authoring,
workflow pinning, grouped/permalinked completed logs, and PR/check status
affordances. This UI audit does not replace the runner safety controls
documented in `actions-public-runners.md`.

For the closeout verdict, remaining parity gaps, and recommended follow-up
sequence, see [actions-ui-parity-closeout.md](./actions-ui-parity-closeout.md)
and [actions-product-polish-closeout.md](./actions-product-polish-closeout.md).

## Current Status

Status: **audit harness in place; S41l landmarks covered by focused render and
security tests.**

The Actions product surface remains server-rendered Go templates with small
vanilla JavaScript islands for the run graph and completed-log helpers. S41l
did not introduce React or a frontend build pipeline. The current UI complexity
is still bounded enough that server templates plus focused JS keep the
security, CSP, dependency, and deploy surface smaller than a React island would.

## Covered Surfaces

Focused Go tests render the production templates for:

- Actions list
- workflow-specific run list
- workflow authoring picker/editor
- viewer-scoped workflow pin controls
- run summary and graph canvas
- completed step logs with grouped sections and line/range anchors
- caches
- attestations
- runners
- usage metrics
- performance metrics
- Code-surface check/status indicators are covered by focused Code-tab tests
  from S41k-7 rather than this screenshot-oriented Actions page harness
- PR/check status affordances are covered by focused PR and Checks tests

The tests assert the main GitHub-parity landmarks: sidebar navigation, workflow
creation, workflow pins, run filters, graph toolbar and popover shell, grouped
completed logs, step-log download controls, PR/check rerun affordances,
management tables, and placeholder empty states.

## Security Checks

The audit tests pin these UI invariants:

- annotation titles/messages are escaped as text;
- workflow, job, step, and log content cannot emit executable markup;
- workflow template filenames stay under `.shithub/workflows/`;
- workflow pin POSTs are CSRF-protected and viewer-scoped;
- completed-log group titles and lines are escaped as text;
- step-log pages link to shithub's internal download route, not raw
  object-storage URLs;
- PR/check templates render only same-repo local Actions details URLs as
  browser links, with rerun forms gated to repo writers;
- Actions templates only reference octicons present in the built-in resolver.

Log masking itself is enforced at runner API ingest, before chunks are stored.
The UI assumes persisted log chunks have already passed through the runner log
scrubber and then escapes them during rendering.

## Screenshot Harness

Run the manual screenshot audit with:

```sh
SHITHUB_URL=https://shithub.sh \
SHITHUB_ACTIONS_REPO=mfwolffe/scratch \
SHITHUB_ACTIONS_WORKFLOW=smoke.yml \
SHITHUB_ACTIONS_RUN=3 \
make audit-actions-ui
```

The harness uses Puppeteer and writes captures under
`.refs/actions-ui-audit/<timestamp>/`. It captures desktop/mobile, dark/light,
and one reduced-motion viewport. The manifest records response status, graph
node counts, log-output presence, and a basic horizontal-overflow check.

## Parked Follow-ups

- Run the screenshot harness against production after each large Actions UI
  merge until the surface stabilizes.
- Add richer keyboard-path assertions once the graph popover needs full
  keyboard manipulation parity with GitHub.
- Add visual diffs only after a deterministic seed repo and browser baseline
  exist in CI.
- Continue follow-ups from the S41k/S41l closeout packets for cache
  management, attestations, runner management UI, richer metrics, live-log
  folding, classic Statuses API compatibility, and marketplace/toolchain parity.
