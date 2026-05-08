# Permissions

Every authorization decision in shithub flows through one function:
`policy.Can(ctx, deps, actor, action, repo) → Decision`. Handlers,
hooks, and the SSH/HTTP git transports all funnel through this single
entrypoint. No surface reads ownership, visibility, or collaborator
state inline; the lint guard at `scripts/lint-policy-boundary.sh`
enforces the boundary in CI.

## The shape

* **Actor** — who's asking. Anonymous, logged-in user (with `IsSuspended`
  and `IsSiteAdmin` flags), or future org-team principal (S31).
* **Action** — what they want to do, drawn from the constant registry
  in `internal/auth/policy/actions.go`. New actions go in their owning
  sprint's PR; the matrix test ensures every constant is covered for
  every actor archetype.
* **Resource** — currently a `RepoRef`; issue/pull/org refs land in
  their owning sprints. The `RepoRef` carries `OwnerOrgID` shape today
  (zero) so S31 plugs in without retro-fitting the interface.
* **Decision** — `{Allow bool; Reason string; Code DenyCode}`. Allow
  drives control flow; Code lets handlers pick a friendly user-facing
  message without re-deriving from the resource fields. Reason is for
  logs and tests, never end-user surfaces.

## Role hierarchy

Five collaborator tiers, mirroring GitHub:

| Role       | Implies                                           | Granted by                |
| ---------- | ------------------------------------------------- | ------------------------- |
| `read`     | Clone/fetch a private repo, view issues/pulls     | invitation by maintainer  |
| `triage`   | read + close/label/assign issues, no code write   | invitation by maintainer  |
| `write`    | triage + push, branch create, PR create/comment   | invitation by maintainer  |
| `maintain` | write + most settings (general, branches)         | invitation by admin       |
| `admin`    | maintain + delete/transfer/visibility/destructive | invitation by admin/owner |

The repo **owner** is implicit `admin` — there's no
`repo_collaborators` row for them. `repo_collaborators` lives in
migration `0019` and is keyed by `(repo_id, user_id)`.

## Action → minimum role table

The complete map (also enforced by the matrix test):

| Action                                | Min role on repo |
| ------------------------------------- | ---------------- |
| `repo:read`                           | `read` (private) |
| `repo:write`                          | `write`          |
| `repo:admin`                          | `admin`          |
| `repo:settings:general`               | `maintain`       |
| `repo:settings:collaborators`         | `admin`          |
| `repo:settings:branches`              | `maintain`       |
| `repo:archive`                        | `admin`          |
| `repo:delete`                         | `admin`          |
| `repo:transfer`                       | `admin`          |
| `repo:visibility`                     | `admin`          |
| `issue:read`                          | `read` (private) |
| `issue:create`                        | `write`          |
| `issue:comment`                       | `write`          |
| `issue:close`                         | `triage`         |
| `issue:label`                         | `triage`         |
| `issue:assign`                        | `triage`         |
| `pull:read`                           | `read` (private) |
| `pull:create`                         | `write`          |
| `pull:merge`                          | `admin`          |
| `pull:review`                         | `write`          |
| `pull:close`                          | `write`          |
| `star:create`                         | logged in        |
| `fork:create`                         | logged in        |

Read actions on **public** repos are short-circuited to allow before the
role check — anyone (anonymous or otherwise) can read a public repo.

## Decision precedence

`Can()` evaluates in a fixed order; the first matching rule produces
the verdict. Ordered from most-decisive to least:

1. **Soft-deleted repo** → deny (`DenyRepoDeleted`). Nothing else
   matters.
2. **Site-admin + read action** → allow. (Write actions still go
   through the rest of the pipeline; broad admin overrides hide bugs
   and create insider-threat surface.)
3. **Suspended actor + write action** → deny (`DenyActorSuspended`).
   Reads against public repos still allowed.
4. **Anonymous + private repo** → deny (`DenyVisibility`). Caller maps
   to 404, not 403, to avoid existence leak.
5. **Public repo + read** → allow.
6. Compute effective role (owner ⇒ admin; collaborator ⇒ row.role;
   else `RoleNone`).
7. **Archived repo + write** → deny (`DenyArchived`). Even owners
   can't push to archived repos.
8. **Min role for action** vs effective role. Below threshold + private
   repo + no role → deny as visibility (404). Below threshold with any
   role → deny as `DenyRoleTooLow` (403).
9. **Login-required actions** (star/fork) on anonymous → deny
   (`DenyAnonymous`).

## Existence-leak guard

`policy.Maybe404(decision, repo, actor)` maps a denial to a status
code that doesn't reveal whether a private repo exists. Convention:

* Allow → 200.
* Deny on a private repo, viewer is not the owner → **404**.
* Deny on a private repo, viewer **is** the owner (e.g. push to
  archived) → **403**, since the owner already knows the repo exists.
* Deny on a public repo → **403**.

Handlers that care about user-facing message tone (e.g. the HTTP git
handler's "repository is archived" stderr line) should switch on
`Decision.Code` rather than parse `Decision.Reason`.

## Per-request memoization

`policy.WithCache(ctx)` attaches a request-scoped memo. The web layer
wires this in `internal/web/middleware/policy.go::PolicyCache`. Within
one request, repeated `Can()` calls for the same `(actor, repo)` pair
hit the cache. Across requests there's no cache — staleness is hard to
get right and the per-request DB cost of fresh lookups is acceptable.

If a handler mutates collaborator state mid-request and re-checks
policy in the same flight, call `policy.InvalidateRepo(ctx, repoID)`
between the mutation and the re-check.

## Suspended actors and the auth surfaces

The `IsSuspended` flag on `Actor` is the canonical input the policy
package uses to deny writes by suspended accounts. Each entrypoint
that constructs an actor must source it correctly:

* **Web (session)** — `middleware.OptionalUser` populates
  `CurrentUser.IsSuspended` from `users.suspended_at`. Handlers pass
  `viewer.IsSuspended` straight into `policy.UserActor`. The lookup
  is run on every request (no cookie-baked state), so an admin
  suspending an account takes effect on the user's next click.
* **Web (PAT)** — `middleware.PATAuthMiddleware` rejects requests
  whose owning user has `suspended_at IS NOT NULL` with a 401 before
  the handler runs. Code paths under PAT auth construct
  `policy.UserActor(..., IsSuspended: false, ...)` because the gate
  is upstream; the field is still passed for honesty and is correct
  by construction.
* **git over HTTPS (`internal/web/handlers/githttp`)** — the basic-
  auth resolver (`auth.go::resolveViaPAT`/`resolveViaPassword`)
  rejects suspended owners with `errBadCredentials` *before* the
  policy check runs, so the `policy.UserActor(..., false, ...)` call
  in `handler.go` never sees a suspended actor. Suspension on the
  HTTPS git path is enforced at credential resolution, not at policy
  evaluation. If the credential resolver is ever reorganised to
  return a populated user even for suspended accounts, propagate the
  flag here.
* **git over SSH (`internal/git/protocol/ssh_dispatch.go`)** — the
  dispatcher loads the user row before constructing the actor and
  passes `user.SuspendedAt.Valid` directly into `policy.UserActor`.
  The `authorized_keys` invocation also rejects up-front (see
  `docs/internal/git-ssh.md`), but the policy call is the
  defence-in-depth layer.
* **post-receive hook (`cmd/shithubd/hook.go`)** — same shape as
  SSH dispatch: load user, pass `SuspendedAt.Valid` into the actor.

When adding a new auth entrypoint (e.g. an OAuth-bearing webhook
ingest), the rule is: load the user record, source `IsSuspended`
from `users.suspended_at`, and *never* hard-code `false`.

## Site-admin scope

`actor.IsSiteAdmin = true` short-circuits to allow on read actions
only. Write actions go through the normal role check, which means a
site admin who is **not** a collaborator on a private repo cannot
push or change settings without explicit impersonation (S34 ships the
impersonation surface). Non-impersonated admin write attempts go
through Can() like any other request and audit-log loudly via the S05
audit recorder.

## Adding a new action

1. Add the constant to `internal/auth/policy/actions.go`.
2. Append it to `AllActions` in the same file.
3. Add a case to `minRoleFor(action)` in `policy.go`. Unknown actions
   default to `RoleAdmin` (deny by default for strangers).
4. The matrix test (`policy_test.go`) iterates `AllActions` and will
   automatically demand a verdict for every actor × resource × this
   action combination. Update `mirrorMinRoleFor` in the test file with
   the same minimum role.

If you add an action that involves a new resource type, add a `*Ref`
struct in `resources.go` and an adapter in `adapters.go`, then
overload `Can` with a new entrypoint (e.g. `CanIssue`, `CanOrg`).

## Boundary lint

`scripts/lint-policy-boundary.sh` runs as part of `make ci`. It fails
when the following patterns appear outside `internal/auth/policy/`,
`internal/repos/`, and `internal/web/handlers/repo/` (which constructs
the policy actor at the lookup wrapper):

* `OwnerUserID == ` or `== row.OwnerUserID` — direct owner equality
* `Visibility == reposdb.Repo...` — direct visibility branching
* `if X.IsArchived` — archived as a control predicate

Test files everywhere are exempt — they legitimately seed state. If a
new pattern surfaces (e.g. an issue handler reads `issue.author_id`),
extend the script accordingly.
