# Repository creation

S11 ships the create-a-repo flow end-to-end: a logged-in user clicks **New**, fills out the form, and lands on the empty-repo home with a quick-setup snippet showing how to push code. The on-disk bare repo is created via S04's `RepoFS.InitBare`; if the user ticked "initialize," a single initial commit is built using git plumbing only — no working tree.

## What's wired

- **Migration:** `0017_repos.sql` adds the `repos` table (with `repo_visibility` enum, owner XOR check, per-owner unique-by-name partial indexes, soft-delete column). `0058_repo_name_reuse_after_soft_delete.sql` narrows those uniqueness indexes to active repos so deleted names can be reused.
- **Source remotes:** `0052_repo_source_remotes.sql` adds one optional public fetch URL per repo. Creation and settings can save this URL, fetch heads/tags, and use it later for submodule gitlink backfill.
- **sqlc package:** `internal/repos/sqlc` (`reposdb`) — Create, Get-by-owner-and-name, Exists, List-by-owner, Count, SoftDelete, UpdateDiskUsed.
- `internal/repos/validate.go` — name shape (≤100 chars, `[a-z0-9._-]`, non-separator edges, no dot-dot, no leading dot) + reserved-name list.
- `internal/repos/templates/` — embeds 10 SPDX licenses + 10 .gitignore templates + a minimal README generator. Sourced from gitea's `options/license` and `options/gitignore` (originally github.com/github/gitignore, MIT/CC0).
- `internal/repos/git/plumbing.go` — `InitialCommit{}.Build(ctx)` runs `git hash-object → update-index → write-tree → commit-tree → update-ref` against a temp index file. No working tree spawned.
- `internal/repos/create.go` — orchestrator: validate → rate-limit → resolve author → tx-insert → InitBare → optional initial commit → audit. Cleans up the on-disk dir on any post-DB failure.
- `internal/web/handlers/repo/repo.go` — GET/POST `/new`, GET `/{owner}/{repo}` (empty home placeholder for S11; S17 will replace).
- `internal/web/templates/repo/{new,empty}.html` + GitHub-aligned CSS.

## Routes

| Route | Method | Handler | Notes |
|---|---|---|---|
| `/new` | GET | `repo.newRepoForm` | Auth-required; renders the form. Optional `owner=<slug-or-token>` preselects an allowed owner. |
| `/new` | POST | `repo.newRepoSubmit` | Auth-required; calls `repos.Create`, redirects on success. |
| `/{owner}/{repo}` | GET | `repo.repoHome` | Two-segment match — does NOT collide with the `/{username}` catch-all. Visibility-aware. |

`/new` is on the reserved-name list so the catch-all profile route can't shadow it. Two-segment `/{owner}/{repo}` doesn't collide with the one-segment `/{username}` route — chi matches by segment count.

## Owner picker

`GET /new` shows the signed-in user's personal namespace plus every
organization where they are allowed to create repositories. Org owners
are always eligible; org members are eligible only when
`allow_member_repo_create` is enabled.

Links from an organization overview use `/new?owner=<org-slug>` so the
form opens with that organization selected. The handler resolves the
hint against the already-authorized owner options, so invalid or
unauthorized hints fall back to the viewer's personal namespace.

## Creation flow

```
POST /new
  │
  ├─ ValidateName / ValidateDescription (friendly error if bad shape)
  ├─ Visibility ∈ {"public", "private"}
  ├─ License/Gitignore keys ∈ curated list (when set)
  ├─ Optional source_remote_url:
  │     normalize + SSRF-validate a public http(s) Git remote
  │     refuse credentials/query/fragment and any init-template combo
  ├─ Limiter.Hit(scope=repo_create, ident=user:<id>, max=10/hour)
  ├─ Resolve author = display name + verified primary email
  │     (refuse with ErrNoVerifiedEmail when init is requested AND missing)
  ├─ RepoFS.RepoPath(owner, name) → defense-in-depth path validation
  ├─ tx.Begin()
  │   ├─ LockRepoOwnerName(owner/name) advisory lock
  │   └─ reposdb.CreateRepo(...)        ← unique-violation surfaces as ErrTaken
  ├─ RepoFS.InitBare(diskPath)          ← `git init --bare --initial-branch=trunk`
  │   └─ if a legacy soft-deleted repo still occupies diskPath:
  │      move it to `.deleted/<old-repo-id>.git` and retry
  ├─ if init flag set:
  │     buildInitialCommit(ic) → commit OID
  │       (hash-object → update-index → write-tree → commit-tree → update-ref)
  ├─ tx.Commit()
  ├─ audit.Record(action=repo_created, target=repo, target_id=<repo.id>)
  ├─ if source_remote_url set:
  │     repo_source_remotes UPSERT
  │     git fetch --no-recurse-submodules heads/tags from that remote
  │     update default_branch/default_branch_oid from fetched refs
  │     enqueue index + size recalculation
  └─ return Result{Repo, InitialCommitOID, DiskPath}
```

Failure handling at each step:

- DB insert error: tx already rolled back via the deferred Rollback closure; nothing on disk to clean.
- FS InitBare error: tx still uncommitted (we Rollback via defer); best-effort `os.RemoveAll(diskPath)` clears any partially-mkdir'd directory. `storage.ErrAlreadyExists` is not blindly removed because it can be a legacy soft-deleted repo path; create first tries to displace that path to the deleted tombstone.
- Initial-commit error: same as above — Rollback + RemoveAll.
- tx.Commit error: post-FS-success but DB couldn't commit. We RemoveAll the bare repo dir to keep DB and disk consistent.
- Audit error: logged at WARN, not propagated — we don't fail the create just because audit logging blipped.
- Source remote fetch error: the repo remains created, the URL is retained with `last_error`, and the user lands on General settings where they can fix or retry the remote.

## Source remotes and imports

Source remotes are for public Git import/mirror metadata, not private
credentials. The accepted shape is `http://` or `https://`, a host, and
a non-empty repository path; userinfo, query strings, and fragments are
rejected so secrets do not enter the database or logs. Before storing or
fetching, the URL runs through `internal/security/ssrf` with DNS
resolution so loopback/private/CGNAT/link-local hosts are rejected.

Fetches use `internal/repos/git.FetchRemoteHeadsAndTags`, which shells
out to canonical git with `--no-recurse-submodules` and non-forcing
head/tag refspecs. If the local branch diverged, git rejects the update;
shithub records the fetch error instead of overwriting local history.
After a successful fetch, shithub keeps the current default branch if it
exists, otherwise prefers `trunk`, then `main`, then `master`, then the
first fetched branch. The chosen branch OID becomes
`repos.default_branch_oid`, making the Code tab and history views work
without a later push.

The same stored remote is used by submodule rendering. If a parent repo
pins a submodule commit that the local target repo lacks, shithub tries
the target repo's source remote before any GitHub-name fallback. This is
the durable path for self-hosted or non-GitHub upstreams: create/import
each submodule repo with its source remote, then create/import the parent
repo, and the pinned submodule links can hydrate exact detached tree
views on demand.

## Plumbing-only initial commit

Why no working tree:

- A working tree means a temp dir, a checkout, an `add`, a `commit`, and cleanup — five orders of magnitude more I/O than what we actually need.
- Plumbing-only is deterministic: same `(name, body)` inputs → same blob OIDs → same tree OID, every time. The test pins `When` and asserts on the resulting commit.
- It's atomic at the ref level: until we run `update-ref`, the bare repo's `HEAD` is an unborn ref pointing at a non-existent branch. Halfway-through state is invisible to clients.

The plumbing helpers shell out to `git` rather than vendoring a Go-native git library. Reasons: (a) any divergence between go-git and real git is a foot-gun; (b) the host requires git ≥ 2.28 anyway for `--initial-branch=trunk`; (c) the call surface is small (4–5 commands) and easy to audit. Future sprints will keep this discipline.

## Templates

License substitution handles the canonical placeholders we encounter in the SPDX texts: `<year>`, `[year]`, `[yyyy]`, `{{ year }}`, `{year}`, plus author flavors `<copyright holders>`, `<owner>`, `<name of author>`, `[fullname]`, `[name of copyright owner]`. Anything we miss survives in the output and is harmless (just less personalized).

The README template is intentionally boring (`# {name}\n\n{description}\n` — nothing more). Per the spec: "always exactly this — no fancy boilerplate."

## Visibility

`/{owner}/{repo}` looks up the row via `GetRepoByOwnerUserAndName` (filters `deleted_at IS NULL`). If the row is `private` and the viewer isn't the owner (or is anonymous), the handler returns `pgx.ErrNoRows` from the lookup helper, which the route catches and renders as 404. This matches GitHub: a private repo is indistinguishable from "doesn't exist."

## Reserved repo names

`internal/repos/validate.go::reservedRepoNames` is the small set of names that would either confuse git itself or break our routing inside the repo URL space. Members: `.git`, `.gitignore`, `.gitmodules`, `.gitattributes`, `.well-known`, `.github`, `head`, `refs`, `objects`, `info`, `hooks`, `branches`. Note: top-level reservations like `new` / `settings` live in `internal/auth/reserved.go` and are checked by the profile route, not here.

## Rate limit

10 creates per rolling hour per user. The throttle key is `repo_create | user:<id>`, namespaced so it never collides with login or signup throttles. Configurable in spirit (the constants live in `internal/repos/create.go`); per-instance overrides land in S15 with the policy package.

## Author identity

We refuse to fabricate a commit author. The user's verified primary email + display name (or username when display name is empty) are baked into the initial commit. Pre-MVP feature: noreply emails for users who want to avoid leaking their address. Today the user must verify their primary email before they can run repo init.

## Testing

`internal/repos/create_test.go` is the integration spine:

- `TestCreate_EmptyRepo` — no init flags. Verifies HEAD is a symbolic ref to `refs/heads/trunk` and the repo has zero commits.
- `TestCreate_WithReadmeLicenseGitignore` — three init flags, `InitialCommitWhen` pinned. Asserts on `rev-list --count = 1`, the `ls-tree` payload, the author identity, and the year substitution in the LICENSE file.
- `TestCreate_RejectsDuplicate` — second create with the same `(owner, name)` returns `ErrTaken`.
- `TestCreate_RejectsReservedName` — name `"head"` returns `ErrReservedName`.
- `TestCreate_RefusesWithoutVerifiedEmail` — user with no verified primary email is rejected with `ErrNoVerifiedEmail` when init is requested.
- `TestCreate_PrivateVisibilityPersists` — visibility round-trips and the disk path lands under the right shard prefix.

`internal/repos/git/plumbing_test.go` — single-commit roundtrip, author env, ref shape.

## Pitfalls / what to remember

- **Tx held across FS operations.** Postgres connection sits idle for a few seconds during InitBare + plumbing. At our scale this is fine; if write throughput grows, swap to a "create row → schedule FS init via a job" pattern.
- **Repo names are lowercased before path construction.** The DB column is `citext` so case-insensitive uniqueness comes for free, but the disk path is always lowercase.
- **Bare repo dirs aren't cleaned on tx.Commit failure** unless we ALSO RemoveAll. The orchestrator does it; future paths that bypass `Create` must remember.
- **Audit row creation is best-effort.** Don't move it inside the tx — an audit failure must not roll back the create.
- **Two-segment route ordering.** `/{owner}/{repo}` is registered before the `/{username}` catch-all but they don't actually conflict (different segment counts). The pattern is preserved for the future when more 2-segment routes (like `/{owner}/{repo}/issues`) ship.
- **License placeholder substitution is best-effort.** We aim for the most common placeholders SPDX uses; anything missed survives in the output.

## Open follow-ups

- **Fork count + `fork_of_repo_id`** are columns now but unused; S27 lights them up.
- **Disk size recalc.** `disk_used_bytes` defaults to 0 and stays there; S14 will enqueue a `repo:size_recalc` job after init.
- **Code listing.** `/{owner}/{repo}` renders the empty placeholder unconditionally; S17 will switch on whether the repo has commits.

## Related docs

- `docs/internal/storage.md` — RepoFS layout + InitBare semantics.
- `docs/internal/auth.md` — login + sessions; restore-on-login affects whether a user can hit `/new`.
- `docs/internal/profile.md` — `/{username}` catch-all that lives next to `/{owner}/{repo}`.
