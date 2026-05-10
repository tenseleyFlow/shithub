# Pull requests

S22 ships the PR core: open, view (Conversation / Commits / Files /
Checks tabs), edit, close/reopen, draft → ready, three merge
strategies (merge / squash / rebase), and auto-synchronize on push.

## Schema (migration 0023)

```
pull_requests          — keyed on issue_id (FK PK to issues)
pull_request_commits   — head-side commit list, refreshed on synchronize
pull_request_files     — changed-files list, refreshed on synchronize
```

Plus four columns on `repos` that S11 left to S22:

| Column                 | Default   | Notes                                              |
| ---------------------- | --------- | -------------------------------------------------- |
| `allow_squash_merge`   | `true`    | UI hides the option when off                       |
| `allow_rebase_merge`   | `true`    | UI hides the option when off                       |
| `allow_merge_commit`   | `true`    | UI hides the option when off                       |
| `default_merge_method` | `'merge'` | preselected in the merge form                      |

A CHECK constraint enforces at least one method enabled so the UI
can never end up unable to merge.

A PR's title / body / state / comments / labels / milestones /
assignees live on the **issues** row (`kind = 'pr'`); the PR-only
fields (base/head refs, draft, mergeable_state, merge metadata) live
on `pull_requests`. Single number space, single comments table —
matches the S21 design.

## Same-repo only (v1)

`head_repo_id` is on the table from day one but always equals
`repo_id` for v1. A CHECK constraint ensures `base_ref <> head_ref`
so a self-merge can't be opened. Cross-fork PRs land in S27.

## Routes

| Route                                                 | Auth          |
| ----------------------------------------------------- | ------------- |
| `GET  /{owner}/{repo}/pulls?state=open\|closed`      | public        |
| `GET  /{owner}/{repo}/pulls/{number}`                 | public        |
| `GET  /{owner}/{repo}/pulls/{number}/files`           | public        |
| `GET  /{owner}/{repo}/pulls/{number}/commits`         | public        |
| `GET  /{owner}/{repo}/pulls/{number}/checks`          | public        |
| `GET  /{owner}/{repo}/pulls/new?base=…&head=…`       | RequireUser   |
| `POST /{owner}/{repo}/pulls`                          | RequireUser   |
| `POST /{owner}/{repo}/pulls/{number}/edit`            | RequireUser   |
| `POST /{owner}/{repo}/pulls/{number}/state`           | RequireUser   |
| `POST /{owner}/{repo}/pulls/{number}/ready`           | RequireUser   |
| `POST /{owner}/{repo}/pulls/{number}/merge`           | RequireUser   |

The compare view (S20) links into `/pulls/new?base=...&head=...` so
the entry point matches GitHub's flow.

## Auto-synchronize on head push

`internal/worker/jobs/push_process.go` is extended: after a push to
`refs/heads/<X>`, list every open PR with `head_repo_id = repo_id`
AND `head_ref = X`, and enqueue `pr:synchronize` for each one. The
synchronize job:

1. Resolves base + head OIDs via `repogit.ResolveRefOID`.
2. Refreshes `pull_request_commits` + `pull_request_files`.
3. Resets `mergeable_state` to `unknown`.
4. Emits a `synchronized` timeline event with the new head OID.
5. Chains a `pr:mergeability` job.

Failure-modes are best-effort — push:process logs but doesn't block
the push pipeline if PR enumeration / enqueue fails.

## Mergeability detection (`pr:mergeability`)

Implemented via `git merge-tree --write-tree --no-messages <base>
<head>` (Git ≥ 2.38). Git auto-computes the merge base.

| Exit | Meaning                                               | mergeable_state |
| ---- | ----------------------------------------------------- | --------------- |
| 0    | Clean merge (TreeOID printed)                         | `clean`         |
| 1    | Conflicts (first line is tree OID, rest are paths)    | `dirty`         |
| ≥ 2  | Real error — wrapped + retried via worker backoff      | unchanged       |

A separate cheap check runs first: if `git log base..head` is empty
the head has no commits ahead and we set `behind` without invoking
merge-tree.

The earlier draft of this code passed `--merge-base=<base>` to
`merge-tree`, which sets the common ancestor *to base itself* and
therefore can never produce a conflict. That was wrong — fixed via
`TestMergeability_Dirty`.

## Merge operation

`internal/repos/git/mergeops.go::PerformMerge` runs entirely
server-side in a temp worktree on the same volume as the bare repo
(S04 mandates `/data/repos/` and tempdirs share the volume). The
worktree is created via `git worktree add --detach <wt> <base_oid>`
so the bare repo's branch refs aren't polluted; only the final
`update-ref` writes back.

Strategies:

- **merge** — `git merge --no-ff --no-edit -m <subject>` against the
  worktree, then update-ref.
- **squash** — `git merge --squash` (stages without committing) then
  `git commit -m <subject>` with author = PR author, committer = merger.
- **rebase** — `git rebase --onto <base_oid> <base_oid> <head_oid>`.
  Each PR commit's author is preserved; committer becomes the merger.

The orchestrator wraps the merge in a tx that holds `SELECT … FOR
UPDATE` on `pull_requests` for the whole job. That row lock
serializes concurrent merges — `TestMerge_RejectsConcurrentDouble`
fans two goroutines and asserts exactly one wins.

`update-ref <ref> <new> <old>` is gated on the expected `<old>` OID
so a concurrent push to base between the merge-tree probe and the
update-ref also fails cleanly.

## Linked-issue auto-close

`Closes/Fixes/Resolves #N` (case-insensitive, all tenses) parsed from:

- The PR body.
- Each head-side commit's subject + body, fetched from
  `pull_request_commits`.

On merge, every linked still-open issue in the same repo flips to
`closed`, `state_reason = 'completed'`, and gets a `closed` event
citing the PR id. Self-references skip silently. The regex lives at
`internal/pulls/linked_issues.go::reLinkedIssue`.

## Identity propagation

Per the spec:

| Strategy | Author                | Committer   |
| -------- | --------------------- | ----------- |
| merge    | merger                | merger      |
| squash   | PR author             | merger      |
| rebase   | original commit author | merger     |

Email = the user's primary verified email. When unset (rare for v1
since repo create requires a verified primary), we synthesize
`<username>@noreply.shithub.local` rather than fail the merge. Real
noreply emails are post-MVP.

## Web UI

- Tabbed view at `/pulls/{number}` switches between Conversation,
  Commits, Files, Checks via the `Tab` field on the template data.
- Conversation follows GitHub's PageHeader + tab strip shape: state
  pill, branch summary, tab count badges, left timeline, right metadata
  rail, and a mergeability box below the timeline.
- Files tab uses the existing S19 diff renderer fed from
  `compareSourceMergeBase` (three-dot diff, base...head).
- The Checks tab is a placeholder — real check runs land in S24.
- The merge form hides disallowed methods and preselects the repo's
  `default_merge_method`.

## Errors

| Error                  | UI message                                                            |
| ---------------------- | --------------------------------------------------------------------- |
| `ErrSameBranch`        | "Base and head must differ."                                          |
| `ErrBaseNotFound`      | "Base branch not found." / "base branch no longer exists" on reopen   |
| `ErrHeadNotFound`      | "Head branch not found." / "head branch no longer exists" on reopen   |
| `ErrNoCommitsToMerge`  | "Head has no commits ahead of base."                                  |
| `ErrAlreadyMerged`     | "already merged"                                                      |
| `ErrAlreadyClosed`     | "already closed"                                                      |
| `ErrMergeBlocked`      | "merge blocked — branch has conflicts or hasn't been checked yet"     |
| `ErrMergeMethodOff`    | "this merge method is disabled on this repo"                          |
| `ErrConcurrentMerge`   | "PR is being merged by another request"                               |

## Tests

| File                                          | Covers                                                       |
| --------------------------------------------- | ------------------------------------------------------------ |
| `internal/pulls/linked_issues_test.go`        | Closes/Fixes/Resolves regex, dedup, prose-vs-keyword         |
| `internal/pulls/pulls_test.go::Create…`       | Issue/PR row pair + initial commits/files snapshot           |
| `internal/pulls/pulls_test.go::Mergeability_Clean` | Probe sets `clean` on simple fast-forward                |
| `internal/pulls/pulls_test.go::Mergeability_Dirty` | Probe sets `dirty` on real shared-file conflict          |
| `internal/pulls/pulls_test.go::Merge_MergeCommit` | Worktree merge writes commit, PR closes with merged_at   |
| `internal/pulls/pulls_test.go::Merge_RejectsConcurrentDouble` | Row lock + update-ref serialize merges        |
| `internal/pulls/pulls_test.go::LinkedIssueAutoClose` | `Fixes #N` in body closes #N on merge                  |

## Pitfalls handled

- **`merge-tree --merge-base=<base>` was wrong** in the early draft;
  it sets the ancestor *to base* and reports clean for any input.
  Drop the flag and let git auto-compute.
- **Non-fast-forward merge commit** — `--no-ff` so we always get a
  merge commit, even when head is strictly ahead of base.
- **Worktree leak on panic** — `defer cleanup()` in `PerformMerge`
  runs `git worktree remove --force` + `os.RemoveAll`. A periodic
  janitor for `<gitDir>/.tmp-worktrees/*` older than 1h is an S37
  follow-up.
- **Squash author mismatch** — author = PR author (looked up via
  `users.id`); committer = merger. Documented in identityFor().
- **Reopen with deleted branches** — `pulls.SetState(open)` validates
  base + head still resolve; otherwise returns `ErrBaseNotFound` /
  `ErrHeadNotFound` for a friendly handler message.
- **Stale mergeability** — synchronize resets to `unknown` so a
  concurrent merge attempt sees the "checking…" banner instead of
  acting on a stale `clean`.
- **Pre-receive bypass on merge** — the internal `update-ref` skips
  the hook by design. Branch protection is checked at the merge
  policy gate (`policy.ActionPullMerge`) instead of via push.

## Deferred

- **Cross-fork PRs** → S27 (forks). The `head_repo_id` column +
  fan-out path are in place; resolver + UI to follow.
- **File-level review threads / approve / request-changes** → S23.
- **Real CI / required-checks gating** → S24. The Checks tab is the
  visual scaffold.
- **Auto-merge / merge-queue** → post-MVP.
- **Inline conflict resolver UI** → post-MVP. v1 surfaces a banner
  with manual-resolve instructions.
- **Auto-delete head branch on merge** → S32 (repo settings) once
  the toggle UI lands. Spec leans opt-in checkbox.
- **`@team` mentions in PR body** → S31.
- **Worktree janitor** → S37.
- **Author-noreply emails** → post-MVP.
- **N+1 on PR list (author, label fan-out)** → S36.
