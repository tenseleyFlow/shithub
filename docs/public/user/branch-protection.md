# Branch and tag protection & reviews

Branch protection lets a repo admin guard specific branches —
typically `main` — from direct pushes, force-pushes, deletion,
and merges that don't meet the team's review/CI bar.

Configured at Repository → Settings → Branches.

The same repository-rules form can target tags. Tag rules use the
same pattern syntax and can block tag deletion, block movement of an
existing tag, and restrict who can push matching tags. Pull request
reviews and status checks are branch-only controls.

## Per-rule controls

A rule applies to one or more branches by name pattern (`main`,
`release/*`, etc.). Each rule can independently:

- **Require pull request before merging** — direct pushes to the
  branch are rejected. The only way in is a merged PR.
- **Require N approvals** — a PR can't merge without that many
  approvals from users with write access.
- **Dismiss stale approvals on new commits** — when the PR head
  moves, prior approvals are wiped.
- **Require approval from someone other than the last pusher** —
  the author of the most recent push to the PR can't be one of
  the approvers.
- **Require conversation resolution** — every line comment must
  be marked "Resolved" before merge.
- **Require status checks** — list of CI check names. The PR's
  head commit must report success on each before merge enables.
- **Require status checks to be up-to-date** — head must include
  the latest base commit before checks count.
- **Restrict who can push** (for non-PR pushes if PRs aren't
  required) — list of users/teams.
- **Disallow force-pushes**.
- **Disallow deletions**.
- **Lock branch** — read-only; no merges either. Used for
  archived release lines.

## How reviews count

- Approvals from users with **read-only** access on the repo
  don't count.
- An approval is dismissed if the reviewer is removed from the
  repo.
- "Request changes" blocks merge until the same reviewer
  re-reviews (Approve or Comment) — a different reviewer's
  Approve does not lift the block.

## Status checks

Status checks come from external CI runners (or, in the future,
shithub's own runner). A check has:

- **Context** — the name (e.g., `ci/lint`, `ci/test`).
- **State** — `pending`, `success`, `failure`, `error`.
- **Description** — short human text.
- **Target URL** — link to the run details.

The first time a context name appears on the repo, you can add
it to the required-checks list. The PR's merge gate evaluates
the **head commit's most recent state per context**.

## Bypass

A repo admin can bypass branch protection for a single push
(per-action; not a permanent setting) — useful for emergencies.
The bypass is recorded in the audit log with the admin's id and
the affected commits.
