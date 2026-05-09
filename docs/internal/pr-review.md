# PR review

S23 ships file-level inline review comments, three review states
(`comment` / `approve` / `request_changes`), reviewer-request
flow, dismiss, server-side draft → submit, branch-protection-driven
required-review gating on merge, and outdated-comment handling on
synchronize.

## Schema (migration 0024)

```
pr_reviews          — one row per submitted review (state, body, dismissed_*)
pr_review_comments  — file-level inline comments anchored on
                      (file_path, side, original_commit_sha,
                       original_line, original_position).
                      review_id NULL + pending=true means draft.
                      review_id N            means part of review N.
                      review_id NULL + pending=false means single inline comment.
pr_review_requests  — pending review requests (user; teams in S31).
```

Plus three columns on `branch_protection_rules`:

| Column                          | Default | Notes                                              |
| ------------------------------- | ------- | -------------------------------------------------- |
| `required_review_count`         | `0`     | How many distinct approves required before merge   |
| `dismiss_stale_reviews_on_push` | `false` | Placeholder; dismiss-on-push wires post-MVP        |
| `require_code_owner_review`     | `false` | Placeholder; CODEOWNERS parsing post-MVP           |

The CHECK on `required_review_count >= 0` keeps the gate sane when an
admin types nonsense.

## Anchoring model

Each comment captures `(file_path, side, original_commit_sha,
original_line, original_position)`. On synchronize, the new helper
`internal/pulls/review/position_map.go::RemapAllForPR` re-walks the
file's content at the new snapshot and tries to match each comment
back to a position. Lines that survive get their `current_position`
updated; lines that don't go to NULL ("outdated").

The v1 mapper is intentionally simple: it reads the file at the new
snapshot and asks "does the original line number still exist?" That
covers ~90% of the additive-commit and pure-rename cases. The
spec calls out `git blame --porcelain` for rebase-heavy PRs; that
swap-in is a follow-up.

Outdated comments still display in the Files tab under a default-
hidden "Show outdated" toggle and stay readable in the Conversation
timeline.

## Server-side drafts

Browser-side drafts are fragile across tabs and don't survive a
crash. The S23 model uses `pending=true` rows on
`pr_review_comments`: every "Add inline comment" action with the
"Pending" checkbox checked stores a draft row with `review_id=NULL`
and `pending=true`. Submitting a review runs

```sql
UPDATE pr_review_comments
SET review_id = $review_id, pending = false
WHERE pr_issue_id = $pr AND author_user_id = $author AND pending = true;
```

inside the same tx as the review insert, so the operation is atomic.

A single inline comment outside a review (the spec's "post directly
without starting a review" path) is `pending=false` from the start,
`review_id=NULL`, and shows up in the diff inline immediately.

## Routes

| Route                                                             | Auth         |
| ----------------------------------------------------------------- | ------------ |
| `POST /{owner}/{repo}/pulls/{n}/review-comments`                  | RequireUser  |
| `POST /{owner}/{repo}/pulls/{n}/review-comments/{cid}/reply`      | RequireUser  |
| `POST /{owner}/{repo}/pulls/{n}/review-comments/{cid}/edit`       | RequireUser  |
| `POST /{owner}/{repo}/pulls/{n}/review-comments/{cid}/resolve`    | RequireUser  |
| `POST /{owner}/{repo}/pulls/{n}/review-comments/{cid}/reopen`     | RequireUser  |
| `POST /{owner}/{repo}/pulls/{n}/reviews`                          | RequireUser  |
| `POST /{owner}/{repo}/pulls/{n}/reviews/{rid}/dismiss`            | RequireUser  |
| `POST /{owner}/{repo}/pulls/{n}/reviewers`                        | RequireUser  |

The settings/branches handler now also accepts
`required_review_count` and `dismiss_stale_reviews_on_push`.

The PR template hides review-request, review-submit, inline-comment,
and thread-resolve controls unless `policy.Can(pull:review)` allows
the viewer. Merge and close controls are similarly driven by
`pull:merge` and `pull:close` decisions. Public viewers who can read a
PR should not see forms that only lead to 403s.

## Required-review gate

`internal/pulls/review/required.go::Evaluate` is the authoritative
gate. It:

1. Looks up the longest-pattern-matching `branch_protection_rule`
   for the PR's `base_ref`.
2. Counts approves per author (latest review per author wins).
3. Counts undismissed `request_changes` reviews.
4. Returns `Satisfied=true` iff `approves >= required_review_count`
   AND no undismissed request_changes.

PR-author approvals are **excluded** from the count.

The gate is consulted at **two points** (belt-and-braces, per spec):

- `pulls.Mergeability` sets `mergeable_state = 'blocked'` when not
  satisfied. Re-runs after every `pr:synchronize`, every review
  submit, and every dismiss.
- `pulls.Merge` re-evaluates the gate inside the `FOR UPDATE` row
  lock right before `repogit.PerformMerge` runs. A `request_changes`
  submitted between the last mergeability tick and the merge attempt
  still blocks.

`mergeable_state` priority order in `Mergeability`:

```
dirty   > behind > blocked > clean
```

## Author cannot approve own PR

Enforced at `review.Submit`: when state is `approve` and the author
matches the PR author, the orchestrator returns
`ErrAuthorCannotApprove` (HTTP 403). The UI also hides the option
in the review form template — server check is authoritative.

## Reviewer requests

Capped at 20 active reviewers per PR (`MaxReviewersPerPR`). Already-
pending reviewer hits `ErrReviewerAlreadyPending` instead of
producing a duplicate row. Submitting an `approve` or
`request_changes` review by a requested reviewer satisfies their
request (`satisfied_by_review_id` is set inside the submit tx); a
plain `comment` review does **not** satisfy, matching GitHub.

## Resolved threads

`Resolve` sets `resolved_at + resolved_by_user_id` on the root
comment; `Reopen` clears them. The Files tab default-collapses
resolved threads under their file section; the comment row picks up
the `shithub-pull-thread-resolved` CSS class so they're visually
distinct.

## Notifications hook

S23 emits these timeline events via `issuesdb.InsertIssueEvent`:

- `reviewed` (with `meta = {state: "approve"|...}`)

Comment-added and review-requested events go through S29's pipeline
when notifications ship; the trigger points are already in place.

## Tests

| File                                              | Covers                                                         |
| ------------------------------------------------- | -------------------------------------------------------------- |
| `internal/pulls/review/review_test.go::AuthorCannotApprove` | Self-approval rejected at orchestrator                |
| `…::AttachesPendingComments`                      | Two `pending=true` drafts attach to a single submitted review  |
| `…::RequiredReviews_BlocksThenUnblocks`           | Rule with `required_review_count=1`: blocked → approve → clean |
| `…::RequestChanges_BlocksMerge`                   | Even without a rule, an undismissed `request_changes` blocks   |
| `…::TwoApprovers_UnblockMerge`                    | `required_review_count=2` satisfied by two distinct authors    |
| `…::ResolveAndReopen`                             | Resolve flips `resolved_at`; double-resolve errors; reopen clears |
| `…::Dismiss_ClearsBlock`                          | Dismissing a `request_changes` re-clears mergeable_state       |

## Pitfalls handled

- **TOCTOU between mergeability and merge** — pre-flight inside the
  row lock re-evaluates the gate. A reviewer hitting
  `request_changes` mid-merge-click can't lose the race.
- **Latest-review-per-author** — a reviewer who first
  `request_changes` and then `approve` counts as approve. A trailing
  `comment` doesn't reset earlier approve. Encoded in
  `latestPerAuthor`.
- **Author email leak** — same caveat as everywhere; reviewer's
  primary verified email is propagated. Documented; noreply is
  post-MVP.
- **`pending=true` orphan rows** — drafts with no submit are user
  state; left in place. A janitor that cleans drafts older than 30d
  is post-MVP.
- **Approver count includes dismissed reviews?** No. The query in
  `ListUndismissedReviewsForGate` filters `dismissed_at IS NULL`
  before the per-author reduce.

## Deferred

- **CODEOWNERS-driven required reviewers** → post-MVP. Column
  exists; parser + UI follow.
- **Auto-dismiss-stale-reviews on synchronize** → post-MVP. Column
  exists; default off matches GitHub.
- **Multi-line comment ranges** → post-MVP.
- **Suggestion-block markdown fence** → post-MVP. Renders as plain
  code today.
- **Cross-fork (S27) reviews** — comments anchor on the head repo's
  OID; cross-repo lookup gap closes when forks land.
- **`@team` review requests** → S31. Column already in place.
- **Position mapping via blame for rebase-heavy PRs** → follow-up.
  Current line-presence mapper is the floor.
- **Notification fan-out** → S29 consumes the events S23 emits.
- **Comment edit on someone else's comment** — not a feature; only
  the author can edit, server-enforced. Repo admins dismiss reviews
  but don't edit comment bodies.
