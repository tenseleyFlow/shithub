# Accessibility audit record

Tracks the findings from the S39 WCAG AA pass and their
disposition (closed / accepted with rationale). Pair with the
tooling under `tests/a11y/` (pa11y-ci + axe-core via Puppeteer)
and the manual screen-reader passes.

> **Status.** This file is the operator log. Entries get added
> as findings come in; nothing here yet because the live audit
> happens against the staging instance, not at code-write time.
> The structure below shows the format the operator uses.

## Audited route set

The S39 acceptance gate is "pa11y reports zero high-severity
issues across the audited route set." Routes under audit:

- Anonymous: `/`, `/signup`, `/login`, `/explore`, `/-/health`
- Authenticated: dashboard, `/settings/profile`,
  `/settings/security/2fa`, `/new`, `/notifications`,
  one repo overview, one issue view, one PR view (with diff),
  one PR review form
- Admin: `/admin/`, `/admin/users`, `/admin/users/{id}`

Specifics for the manual SR pass on top of the automated runs:

- Diff view labelling old/new sides for SR users.
- Modal dialogs (delete-repo confirm, transfer-repo confirm,
  rotate-secret confirm) trap focus and announce on open.
- Form errors associated with their fields via `aria-describedby`.
- Tables (issue lists, PR lists, audit log) have proper `<th
  scope>` headers.
- Keyboard order matches visual order on every form.

## Findings template

Each finding is one row:

```
### F-NN — <short title>

- **Found by:** pa11y / axe / manual SR / manual keyboard / dev review
- **Route:** /…/…
- **Tool rule (if automated):** WCAG2AA.<...>
- **Impact:** critical / serious / moderate / minor
- **Description:** what's wrong, in one paragraph.
- **Disposition:** fixed in <commit-sha> / accepted: <rationale> / deferred to <sprint>
- **Re-tested on:** <date>
```

## Dispositions accepted with rationale

These are findings we acknowledge but do not fix in S39:

(none yet)

## Manual SR notes

NVDA + Firefox / VoiceOver + Safari — keep notes here so we don't
re-discover the same SR-readability nuances across sprints.

(none yet)

## CI integration

The `audit-a11y-pa11y` Makefile target runs pa11y-ci against the
URL list. Hooked into a manual-trigger CI job (not the main `ci`
target — it needs a running shithub on the runner, which the
default CI environment doesn't provide). The run produces the
findings list that gets transcribed into this file.

## Re-audit cadence

- Every sprint that touches `internal/web/templates/` or
  `internal/web/static/css/`.
- Every release that adds a new top-level route.
- Quarterly full audit (matches the security re-audit cadence).
