# Pull requests

A pull request (PR) proposes merging one branch into another.
The PR is the conversation around the diff — review comments,
status checks, the merge itself.

## Opening a PR

1. Push a branch to the repo (or to your fork).
2. Visit the repo and click the "Compare & pull request" prompt
   that appears, or go to Pull requests → "New pull request".
3. Pick **base** (where you want to merge into) and **compare**
   (your branch).
4. Title + body. Markdown supported. Reference issues with
   `Fixes #N` to auto-close on merge.

The PR shows the file diff, commit list, and any pre-existing
status checks for the head commit.

## Reviewing

Open a PR → "Files changed" tab.

- **Comment on a line:** click the `+` in the gutter.
- **Suggest a change:** in the comment, pick "Suggestion" — your
  inline patch becomes a one-click commit the author can apply.
- **Submit a review:** "Review changes" → choose:
  - **Comment** — observations, no verdict.
  - **Approve** — green checkmark, counts toward the required-
    reviewer threshold (if branch protection is on).
  - **Request changes** — red X, blocks merge until dismissed or
    re-reviewed.

A reviewer can leave many inline comments and bundle them into
one review submission.

## Merging

Three merge methods (the maintainer picks per repo or per PR if
multiple are enabled):

- **Merge commit** — the classic non-fast-forward merge with a
  commit that has both parents. Preserves all PR commits as-is.
- **Squash and merge** — replaces every PR commit with one
  squashed commit on the base branch. PR title/body becomes the
  commit message.
- **Rebase and merge** — replays the PR's commits onto the base.
  No merge commit; linear history.

shithub detects merge conflicts up front and prevents merge
until the PR head is updated. Use the "Update branch" button to
merge or rebase the base into your branch (the maintainer picks
which strategy is allowed).

## Branch protection gates

If the base branch is protected, the merge button is disabled
until:

- Required status checks pass (CI integrations report success).
- Required number of approvals collected.
- "Request changes" reviews resolved (dismiss or re-review).
- Conversations resolved (if "Require conversation resolution" is on).

See [Branch protection & reviews](./branch-protection.md).

## Draft PRs

Open as draft to signal "not ready yet, but visible". Drafts
cannot be merged. Convert to "Ready for review" when you want
reviewers.

## After merge

- The head branch can be auto-deleted (account-level + per-repo
  setting).
- Linked issues with `Fixes #N` close automatically.
- Watchers + subscribers get notifications.
