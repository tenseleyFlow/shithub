# Accessibility audit

This directory contains the tooling for the WCAG AA audit pass
described in S39. Two complementary tools:

- **pa11y-ci** — broad scan across anonymous routes. Runs from a
  static URL list in `pa11y-config.json`. Cheap to run, good for
  catching regressions on the main pages.
- **axe-core via Puppeteer** — focused scan on the routes that
  need a logged-in session (settings, dashboard, new-repo form,
  notifications). Drives a headless Chromium through a real
  sign-in.

Automated tools catch ~30% of accessibility issues. Pair these
with manual screen-reader passes (NVDA on Windows / VoiceOver on
macOS) and keyboard-only navigation.

## Prerequisites

```sh
# One-time, in your CI runner or workstation:
npm i -g pa11y-ci puppeteer @axe-core/puppeteer
```

You also need a running shithub instance with seeded data. For
local runs:

```sh
make dev-db dev-storage dev-migrate dev
# in another terminal, sign up an account so the auth flow has
# a known credential. Then export it:
export SHITHUB_USER=alice
export SHITHUB_PASS=<the-password-you-set>
export SHITHUB_URL=http://127.0.0.1:8080
```

## Running

```sh
# Anonymous routes:
make audit-a11y-pa11y

# Authenticated + diff/issue views:
make audit-a11y-axe

# Both:
make audit-a11y
```

Both tools exit non-zero on a high-severity (critical / serious)
violation. CI gates on a clean run; lower-severity findings go to
the audit record (`docs/internal/a11y-audit-record.md`).

## What's covered

| Tool         | Scope                                                |
|--------------|------------------------------------------------------|
| pa11y-ci     | `/`, `/signup`, `/login`, `/explore`, `/-/health`    |
| axe-runner   | dashboard, settings/profile, settings/security/2fa, `/new`, `/notifications` |
| Manual SR    | every primary form + diff view + admin surfaces      |

## What's NOT covered by automation

- **Screen-reader semantics** that pass programmatic checks but
  read poorly. Diff view labelling old/new for SR users is the
  canonical example — `aria-label` checks pass; whether the
  resulting verbal output makes sense needs a human.
- **Keyboard order** that diverges from visual order in
  CSS-positioned layouts. `tabindex` audits help but don't catch
  every case.
- **Modal focus trapping** under unusual interaction sequences.
- **Color contrast** in user-content (avatars, custom topic colors).

Findings from the manual audits are recorded in
`docs/internal/a11y-audit-record.md` along with the dispositions
(fixed / accepted-with-rationale).
