<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Actions UI Screenshot Audit

This harness captures the S41k/S41l Actions parity surfaces against a running
shithub server. It is intentionally operator-run instead of CI-required:
screenshots depend on seeded repositories, real workflow runs, browser fonts,
and light/dark rendering.

## Prerequisites

- shithub web server running locally, on staging, or on production
- `puppeteer` available to Node, for example `npm i -g puppeteer`
- a repository with at least one `.shithub/workflows/*.yml` workflow
- optionally, a completed run index for the run summary and step log pages

## Example

```sh
SHITHUB_URL=https://shithub.sh \
SHITHUB_ACTIONS_REPO=mfwolffe/scratch \
SHITHUB_ACTIONS_WORKFLOW=smoke.yml \
SHITHUB_ACTIONS_RUN=3 \
make audit-actions-ui
```

If the target requires authentication, also set `SHITHUB_USER` and
`SHITHUB_PASS`. Screenshots and `manifest.json` are written under
`.refs/actions-ui-audit/<timestamp>/` by default so private captures are not
committed.

## Coverage

The default route set captures:

- Actions list
- New workflow route when passed through `SHITHUB_ACTIONS_AUDIT_ROUTES`
- workflow-specific run list
- run summary and graph canvas, when `SHITHUB_ACTIONS_RUN` is set
- step log, including completed-log group/permalink landmarks when the selected
  run contains grouped logs
- caches
- attestations
- runners
- usage metrics
- performance metrics

Each route is captured in desktop dark, desktop light, mobile dark, and mobile
light with reduced motion. The manifest also records response status, basic
horizontal overflow checks, graph-node counts, and whether log output is
present.

Use `SHITHUB_ACTIONS_AUDIT_ROUTES` for one-off routes:

```sh
SHITHUB_ACTIONS_AUDIT_ROUTES='new=/mfwolffe/scratch/actions/new,wide=/mfwolffe/scratch/actions/runs/9' make audit-actions-ui
```
