# Repo lifecycle

Post-creation a repo can be **renamed**, **transferred** to another
user, **archived** (read-only), have its **visibility** flipped,
**soft-deleted** (with a 7-day grace), and finally **hard-deleted** by
a worker. Every transition is a function in `internal/repos/lifecycle/`;
the corresponding handler in `internal/web/handlers/repo/lifecycle.go`
runs the policy gate and dispatches.

Each operation is described below with: who can do it, what state
changes, what the recovery semantics are if a step partway through
fails, and what audit row gets emitted.

## Rename

`POST /{owner}/{repo}/settings/rename` → `lifecycle.Rename`.

* **Auth**: `repo:admin` action via S15 policy. Owner-only in V1
  (collaborator admins can call it via the same gate; nothing in the
  policy table is owner-specific).
* **Rate limit**: 5 renames per 30 days. Tracked by counting redirect
  rows for this repo within the window. Exceeding returns
  `ErrRenameRateLimited` → HTTP 429.
* **Steps**:
  1. Validate the new name (regex, reserved list, lowercase).
  2. Open a transaction.
  3. INSERT `repo_redirects(old_owner_user_id, old_name, repo_id)`.
  4. UPDATE `repos.name`. A unique-violation rolls back both → user
     sees `ErrNameTaken` (409).
  5. COMMIT.
  6. `RepoFS.Move(/data/repos/sh/shithub/old.git, /data/repos/sh/shithub/new.git)`.
* **FS-failure recovery**: a compensating UPDATE reverts `repos.name`
  and DELETEs the redirect row, so persistent state remains
  consistent. The user-facing error reflects the FS failure (which is
  the truth — the move didn't happen).
* **Audit**: `repo_renamed` with `{old_name, new_name}`.

## Transfer

* **Initiate**: `POST /{owner}/{repo}/settings/transfer` →
  `lifecycle.RequestTransfer`. Sender confirms by typing the current
  `owner/repo` slug. Inserts a `repo_transfer_requests` row with
  `expires_at = now() + 7 days`, status `pending`.
* **Accept**: `POST /transfers/{id}/accept` → `lifecycle.AcceptTransfer`.
  The recipient is the actor. Atomic transaction:
  1. `SELECT … FOR UPDATE` on the transfer row to serialize against
     concurrent decline / cancel.
  2. INSERT redirect from old owner+name.
  3. UPDATE `repos.owner_user_id` to the recipient (name unchanged).
  4. UPDATE the transfer row to status `accepted` with timestamp.
  5. COMMIT.
  6. `RepoFS.Move` from old owner's shard to recipient's shard. FS
     failure here is logged but doesn't roll back — the DB is the
     source of truth.
* **Decline**: `POST /transfers/{id}/decline`. Recipient flips status
  to `declined`. Repo state unchanged.
* **Cancel**: `POST /transfers/{id}/cancel`. Repo admin (sender)
  flips status to `canceled`. Recipient's accept attempts return
  `ErrTransferTerminal` (409).
* **Expire**: pending transfers past `expires_at` are flipped to
  `expired` by the `lifecycle:sweep` worker job. Bulk operation; one
  audit row would be noisy, so the row's terminal status is the audit.

## Archive / Unarchive

* `POST /{owner}/{repo}/settings/archive` → `is_archived=true`,
  `archived_at=now()`. Does **not** move FS or change visibility.
* `POST /{owner}/{repo}/settings/unarchive` → reverse.
* **Push behavior**: enforced by S15's archived-repo deny in
  `policy.Can` (`DenyArchived`). HTTP / SSH git transports both
  surface the friendly "repository is archived; pushes are disabled"
  message.

## Visibility

`POST /{owner}/{repo}/settings/visibility` →
`lifecycle.SetVisibility(public|private)`. Idempotent on the same value.
Changing public → private only stops *future* anonymous reads — already-
fetched clones aren't recallable, that's git's nature. The UI surfaces
this caveat explicitly.

Existing **forks remain independent** — they're separate repos at the
data layer; visibility flips don't cascade.

## Soft delete + restore + hard delete

* **Soft delete**: `POST /{owner}/{repo}/settings/delete` →
  `lifecycle.SoftDelete`. Sets `deleted_at = now()`. Repo disappears
  from listings, profile pinned slot, search. The bare repo is moved
  from the canonical `<owner>/<name>.git` path to an internal
  `.deleted/<repo-id>.git` tombstone so a fresh repo can reuse the same
  owner/name during the grace window. `/{owner}/{repo}` 404s for
  non-owners (auth-aware via S15 policy's `DenyRepoDeleted`).
* **Restore**: owner sees the soft-deleted repo at
  `/settings/repositories`. POST to
  `/settings/repositories/restore/{id}` moves the tombstone back to the
  canonical path and clears `deleted_at`. If the owner/name was reused,
  restore returns `ErrNameTaken` and leaves the deleted repo in the
  restore list. Past the 7-day grace, restore returns `ErrPastGrace`
  (410).
* **Hard delete**: `lifecycle:sweep` worker job (registered in
  `cmd/shithubd/worker.go`) runs periodically. The handler:
  1. `ListRepoIDsPastSoftDeleteGrace` — finds rows past 7 days.
  2. For each, `lifecycle.HardDelete` runs:
     * `OrphanForksOf(repoID)` — children's `fork_of_repo_id` set NULL.
     * `DELETE FROM repos WHERE id=$1`. FK ON DELETE CASCADE handles
       `push_events`, `repo_collaborators`, `repo_redirects` (rows
       pointing at this repo), `repo_transfer_requests`, and
       `webhook_events_pending`.
     * Remove the tombstoned bare repo on disk. Legacy soft-deleted
       repos whose bare data still sits at the canonical path are
       removed only when no active repo has reused the owner/name.
     * Audit `repo_hard_deleted` with the row snapshot in `meta`,
       since the repo_id no longer resolves.
  3. The same job also flips pending transfers past their TTL via
     `ExpirePending`.

## Redirect lookup on `/{owner}/{repo}`

The `repoHome` handler now consults `repo_redirects` when
`lookupRepoForViewer` returns ErrNoRows:

* Hit → 301 to the canonical `/{currentOwner}/{currentName}` (with
  any path tail preserved for future `/blob/x` and similar).
* Miss → 404 as before.

A redirect row pointing at a soft-deleted repo is treated as a miss
to avoid 301 chains into 404s. When the hard-delete worker runs, the
redirect row is removed by FK cascade and the URL transitions cleanly
from "redirected" to "never existed."

## Operational pointers

* The `lifecycle:sweep` worker is enqueued ad-hoc today; S37 ships
  cron scheduling. To run it manually:
  ```sql
  INSERT INTO jobs (kind, payload) VALUES ('lifecycle:sweep', '{}'::jsonb);
  NOTIFY shithub_jobs;
  ```
* Soft-deleted repos keep their bare data for the full grace window,
  but under the owner-local `.deleted/<repo-id>.git` tombstone path
  rather than the active canonical path. If disk pressure becomes an
  issue, configure the grace window down (it's a constant in
  `lifecycle.go::softDeleteGrace`; promote to config in S37 if needed).
* Renaming a repo doesn't require restarting any worker or hook.
  The atomic FS move + DB update means the next hook invocation will
  resolve the new path.

## Audit actions emitted

| Action                          | When                              |
| ------------------------------- | --------------------------------- |
| `repo_renamed`                  | rename completes                  |
| `repo_archived`                 | archive                           |
| `repo_unarchived`               | unarchive                         |
| `repo_visibility_changed`       | flip                              |
| `repo_soft_deleted`             | soft delete                       |
| `repo_restored`                 | restore                           |
| `repo_hard_deleted`             | hard-delete worker — meta carries snapshot |
| `repo_transfer_requested`       | offer created                     |
| `repo_transfer_accepted`        | recipient accepts                 |
| `repo_transfer_declined`        | recipient declines                |
| `repo_transfer_canceled`        | sender cancels                    |

## Deferred work (S16 → later sprints)

Two pieces of S16's spec are intentionally deferred; the receiving
sprints have explicit bullets so the work doesn't fall off:

* **Email notifications** for archive / unarchive / visibility flip /
  soft-delete / restore / transfer requested / accepted / declined /
  canceled / expired → tracked in S29 (notifications). The audit rows
  already exist; S29's `domain_events` table is the consumer.
* **Periodic enqueue of `lifecycle:sweep`** → tracked in S37 (deploy
  automation), under `shithubd-cron.timer`. Today operators kick the
  job manually with the SQL above.
