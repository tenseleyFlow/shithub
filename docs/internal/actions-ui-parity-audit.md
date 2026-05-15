# Actions UI Parity Audit

This document records the S41k audit harness for the repository Actions UI.
S41k is the product/UI parity track; it does not replace the runner safety
controls documented in `actions-public-runners.md`.

For the closeout verdict, remaining parity gaps, and recommended follow-up
sequence, see [actions-ui-parity-closeout.md](./actions-ui-parity-closeout.md).

## Current Status

Status: **audit harness in place; closeout packet added in S41k-8**.

The Actions product surface remains server-rendered Go templates with a small
vanilla JavaScript island for the run graph. S41k did not introduce React or a
frontend build pipeline. The current UI complexity is still bounded enough that
server templates plus focused JS keep the security, CSP, dependency, and deploy
surface smaller than a React island would.

## Covered Surfaces

Focused Go tests render the production templates for:

- Actions list
- workflow-specific run list
- run summary and graph canvas
- step log
- caches
- attestations
- runners
- usage metrics
- performance metrics
- Code-surface check/status indicators are covered by focused Code-tab tests
  from S41k-7 rather than this screenshot-oriented Actions page harness

The tests assert the main GitHub-parity landmarks: sidebar navigation, run
filters, graph toolbar and popover shell, step-log download controls, management
tables, and placeholder empty states.

## Security Checks

The audit tests pin these UI invariants:

- annotation titles/messages are escaped as text;
- workflow, job, step, and log content cannot emit executable markup;
- step-log pages link to shithub's internal download route, not raw
  object-storage URLs;
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
- Continue follow-ups from the S41k-8 closeout packet for workflow authoring,
  pinning, cache management, attestations, runner management UI, richer metrics,
  log folding/permalinks, PR/checks parity, classic Statuses API compatibility,
  and marketplace/toolchain parity.
