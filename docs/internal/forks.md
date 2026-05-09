# Forks (S27)

S27 ships fork creation, fork sync (fast-forward only), ahead/behind
stats, and the schema columns + triggers that maintain
`repos.fork_count`. Cross-fork PRs and the S16 hard-delete-cascade
amendment for repacking forks are scoped here too; the cross-fork PR
deferral pointer and the S16 amendment are landed in their own
sub-sections.

## Schema

`repos` gained two columns in 0029:

* `fork_count bigint NOT NULL DEFAULT 0` — maintained by the
  `forks_count_inc` / `forks_count_dec` AFTER triggers on `repos`
  insert/delete. Decrement uses `GREATEST(... - 1, 0)` so a
  hand-written DB tweak that violates the trigger doesn't
  underflow into negatives.
* `init_status repo_init_status NOT NULL DEFAULT 'initialized'` —
  enum `('initialized', 'init_pending', 'init_failed')`. Synchronous
  repo creates (the S11 path) write `'initialized'` directly. Forks
  start at `'init_pending'`; the worker job flips to `'initialized'`
  on success or `'init_failed'` on permanent failure.

`fork_of_repo_id` was already present from S11 (the only column the
S11 status block actually shipped — `is_fork` and `fork_count` were
the missed ones, same shape as the S11/S26 gap noted in the
stars-watchers doc).

We deliberately did NOT add `is_fork` — it would duplicate
`fork_of_repo_id IS NOT NULL` and create the kind of two-source-of-
truth drift that the audit penalises. Use the FK predicate.

## On-disk layout

`git clone --bare --shared <source> <fork>` creates a fork whose
`objects/info/alternates` file points back at the source's `objects/`
directory. Disk usage of the fork is essentially refs + a small
overhead. The same-volume requirement (S04 `RepoFS.Root()`) is what
makes alternates safe — alternates across volumes is undefined
behaviour for git.

When a fork is created we additionally set
`extensions.preciousObjects = true` on the **source** so a future
`git gc` on the source can't prune objects the fork reaches via
alternates. Idempotent; the fork-clone worker re-asserts on every
new fork so missing config is self-healing.

## Worker job

`KindRepoForkClone` (`internal/worker/jobs/repo_fork_clone.go`) runs
the on-disk clone out of band so fork-create returns fast even for
large source repos. Payload is `{source_repo_id, fork_repo_id}`.

The job's flow:

1. Reload both repos by id (defends against soft-delete between
   enqueue and run).
2. `CloneBareShared(sourcePath, forkPath)` — git clone + alternates.
3. `hooks.Install(forkPath, shithubdPath)` — same hook install as
   the synchronous repo-create path so subsequent user pushes fire
   `push:process`.
4. `SetPreciousObjects(sourcePath)` — pin source's objects.
5. `SetRepoInitStatus(fork.ID, 'initialized')`.

On any permanent failure: flip to `'init_failed'` and return
`worker.PoisonError` (no retries). The repo row stays so the user
sees the failure; we don't auto-cleanup because that races concurrent
retries.

## Sync (fast-forward fork from upstream)

`fork.Sync(ctx, deps, actorUserID, forkRepoID)` only fast-forwards.
Anything else (merge, rebase) belongs in the user's client; doing
either server-side without the user's resolution preferences risks
producing commits the user doesn't want.

Algorithm:

1. Resolve both default-branch OIDs (`fork`, `source`).
2. If equal → `ErrSyncUpToDate`.
3. If fork is NOT an ancestor of upstream → `ErrSyncDiverged`.
4. CAS update via `repogit.UpdateRefCAS(fork, branch, upstream, fork)`
   — the trailing `fork` argument is git's old-value guard. A
   concurrent push to the fork loses the CAS and surfaces as
   `ErrSyncRefRaced`.
5. Update `repos.default_branch_oid` so the home view reflects the
   new tip without waiting for `push:process` (update-ref bypasses
   `post-receive`, same shape as the merge handler's fix in the
   audit-remediation sprint).

Empty fork (no branch yet) is handled via the 40-zero OID literal
that git accepts as "ref must not exist yet" semantics — sync to
an empty fork creates the branch from upstream's tip.

## Ahead/behind

`fork.AheadBehind(ctx, deps, forkRepoID)` returns
`{Ahead, Behind, Comparable}` where:

* `Ahead` = commits in fork's default branch not in source's.
* `Behind` = commits in source's default branch not in fork's.
* `Comparable` = false when either side's default ref is missing
  (empty fork, never-initialised source).

Implementation: read both OIDs, then run
`git rev-list --left-right --count` *inside the fork's repo*.
Because the fork shares object alternates with the source, the
upstream OID resolves without an explicit fetch.

This is the floor implementation. S36's perf-pass sprint adds an
LRU cache keyed on `(fork_repo_id, fork_default_oid,
upstream_default_oid)` — already documented in S36's "Code-tab
caching" deliverables and on the S00–S25 audit's H4 deferral.

## Visibility floor

`fork.allowedTargetVisibility(source, target)` enforces:

| source  | target=public | target=private | target="" |
|---------|---------------|----------------|-----------|
| public  | ✓             | ✓              | public    |
| private | ✗             | ✓              | private   |

Forking private → public would expose previously-private content
and is always rejected (`ErrVisibilityFloor`).

## Permission lattice

`policy.ActionForkCreate` was already in the registry from earlier
sprints. Today's gating shape:

* Anonymous on any repo → deny (`DenyAnonymous`).
* Logged-in on a repo they can read → allow (login-required, no
  role gate).
* Logged-in on a private repo they CAN'T read → deny
  (`DenyVisibility`, leaks as 404 at the handler layer).

Suspended actors are blocked by step 3 of `policy.Can` (suspended +
write action → deny). Fork create counts as write (it mutates the
target owner's namespace).

## Cross-fork PRs (deferred to a follow-up)

S27's spec lists cross-fork PR support as in-scope, but the actual
plumbing — fetching the fork's head into the base repo's
`refs/shithub-pr/<pr_id>/head` namespace and routing the merge from
the internal ref — is large enough that this sprint ships fork
creation, sync, and ahead/behind only. The cross-fork PR work is
tracked here as a follow-up:

* Extend `pulls.Create` to accept `head_repo_id != repo_id`.
* Add `repogit.FetchIntoNamespace` (already shipped in this sprint
  for the eventual consumer).
* `pulls.Synchronize` reads head from the internal ref when
  `pull_request.head_repo_id != pull_request.repo_id`.
* `pulls.Merge` worktree-add reads head from the internal ref.
* Re-check fork visibility at merge time (the merger may have lost
  read access on the head between PR open and merge).

The internal ref is private — we never advertise it via
`info/refs`. The git-http handler's ref filter already restricts
to `refs/heads/*` and `refs/tags/*`, so the namespace is
naturally hidden.

## S16 hard-delete cascade amendment

When a source repo with active forks is hard-deleted, the forks
become orphans (`fork_of_repo_id ON DELETE SET NULL` from the
existing FK). Today the orphan forks have only the *refs* they
added since fork — the objects up to fork point still live in the
source. Hard-deleting the source would prune those objects and
break the orphan forks.

The fix is to repack each fork before removing the source:

```
git repack -a -d --no-shared
```

…runs in the fork's repo, copies all reachable objects into the
fork's own pack, then we can safely delete the source.

This is a `KindRepoForkRepackOnSourceDelete` job (deferred from
S16; see `ListForksOfRepoForRepack` query that this sprint shipped
for it). The lifecycle worker's `repo_hard_delete` step needs to
fan out one repack job per fork, await completion, then proceed
with the FS delete.

The query is in place; the job + the cascade wiring land in a
follow-up commit (or in S37 when the deploy plan freezes the
hard-delete sequence).

## Routes

| Method | Path                                | Auth          | Notes                              |
|--------|-------------------------------------|---------------|------------------------------------|
| POST   | `/{owner}/{repo}/fork`              | RequireUser   | Create a fork                      |
| POST   | `/{owner}/{repo}/sync`              | RequireUser   | Fast-forward fork from upstream    |
| GET    | `/{owner}/{repo}/forks`             | public        | Paginated list of forks            |

The `/fork` POST emits a `forked` domain event (kind=`forked`,
source_kind=`repo`) into S26's `domain_events` log so the future
activity feed picks it up. The `/sync` POST emits `repo_fork_synced`
through the audit log only (no public event).

The fork-create handler also auto-watches the new fork at
`level=all` so the user sees fork-side events without having to
opt in. Matches GitHub's "watching your own forks" default.

## Pitfalls noted in code

* Source-repo GC pruning fork-needed objects → `preciousObjects`.
* Source-repo deletion with active forks → S16 amendment (above).
* Cross-fork PR with deleted fork → mark
  `mergeable_state='blocked'` with "head repository deleted"
  reason at the merge gate (lands with cross-fork PR work).
* Fork rename / transfer → `fork_of_repo_id` is by-id so the
  relationship survives.
* Sync race with concurrent push → CAS on update-ref; surfaces as
  `ErrSyncRefRaced`.
* Fork-of-fork chains → spec leans "flatten alternates to root".
  Today the clone uses `--shared` against whatever path we pass; if
  the source is itself a fork, the alternates chain is two levels
  deep. Acceptable for v1; the flattening lands when fork-of-fork
  becomes a real user complaint.
