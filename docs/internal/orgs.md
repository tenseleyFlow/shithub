# Organizations (S30)

Organizations as first-class repo owners. The `principals` table
unifies `/{slug}` resolution across users + orgs so a slug collision
is structurally impossible at the DB layer.

## Schema (migration 0034)

```
orgs              — slug citext, plan, allow_member_repo_create, suspended/deleted
org_members       — (org_id, user_id) → role enum('owner','member')
org_invitations   — pending invites by username OR email; HMAC-hashed tokens
principals        — (slug citext PK, kind enum('user','org'), id) maintained
                    by AFTER triggers on users + orgs
```

The `repos.owner_org_id` FK to `orgs.id` is added now (the column was
already present from 0017 with the XOR CHECK).

## Routing

```
GET  /organizations/new            create form (auth required)
POST /organizations                create submit
GET  /{slug}                       /{user-or-org} — dispatched via principals.Resolve
POST /{slug}/pins                  owner-only org profile pin customization
GET  /{org}/people                 members + (owner-only) invite form
POST /{org}/people/invite          invite by username OR email
POST /{org}/people/{userID}/role   change role (owner-only)
POST /{org}/people/{userID}/remove remove member (owner-only)
GET  /invitations/{token}          accept/decline view (auth required)
POST /invitations/{token}/accept
POST /invitations/{token}/decline
```

## Organization profile

`GET /{slug}` renders a GitHub-style organization overview when
`principals.Resolve` returns `kind='org'`.

The overview data is built in `internal/web/handlers/profile`:

* org identity header with slug, display name, description, location,
  website, avatar fallback, and owner/member view state.
* org underline nav with Overview active, repository and member counts,
  links to the shipped people/teams surfaces, and disabled parity tabs
  for deferred GitHub org sections.
* pinned repo cards backed by `profile_pin_sets` / `profile_pins`
  after an owner customizes them. Until then, the overview falls back
  to public org repos sorted by stars and recent update time.
* recent visible repositories, sorted by `updated_at`, with visibility
  badges, language, license, star/fork counts, topics, update time,
  and a read-only weekly commit-activity sparkline for the default branch.
* right rail aggregates for people, top primary languages, and most
  used topics.

Owner/member viewers who can create repositories see org homepage
**New** links to `/new?owner=<org-slug>`. The repo-create handler only
honors that hint after matching it against the viewer's allowed owner
picker entries, so unauthorized org hints fall back to the viewer's
personal namespace.

`GET /{org}/people` uses the same GitHub-style org pagehead and
underline navigation, then renders the People surface as a permissions
layout: a left-side "Organization permissions" menu, a member search
toolbar, bordered member rows with avatars, and an owner-only Invite
member action. The `query` URL parameter filters members by username,
display name, or role without changing the membership management
routes.

Organization owners see a "Customize pins" modal on the overview. The
picker mirrors GitHub's public-profile rule: it offers only public
org-owned repos, has a live text filter, caps selections at six, and
persists the ordered set transactionally. Saving no selected repos is a
real customized state and suppresses the automatic fallback.

Repo visibility is filtered through `policy.IsVisibleTo` using an actor
constructed from `middleware.CurrentUser`, including suspension,
site-admin, and impersonation write-mode fields. Anonymous viewers only
see public repositories; members and owners see whatever the policy
layer grants them.

There is no dedicated `/orgs/{org}/repositories` page yet. The Overview
nav's Repositories item anchors to the homepage repository list until a
full org repositories tab lands.

`/{slug}` resolution flow inside `internal/web/handlers/profile/profile.go`:

1. Reserved-name check (defense in depth — chi already matches static
   routes first).
2. `orgs.Resolve(slug)` against `principals`. On `kind='org'`, dispatch
   to `serveOrgProfile`. On `kind='user'`, fall through to the
   existing user path. On miss, fall through to `username_redirects`
   for renamed users.

## Member roles

* `owner` → implicit `admin` on every org-owned repo through
  `policy.Can`.
* `member` → org-membership badge; no implicit access to private repos.
  Repo-level access is granted via direct collaboration (S15) or
  teams (S31).

## Last-owner protection

`ChangeRole` and `RemoveMember` both call `CountOrgOwners` and refuse
the operation if it would leave the org with zero owners
(`ErrLastOwner`). UI must surface this; the orchestrator is the
canonical enforcer.

## Invitation flow

* By **username**: orchestrator resolves the username → user_id and
  checks for existing membership before issuing the invite.
* By **email**: stores the email; recipient claims by signing in with
  any account that owns the verified email (or by signing up later
  with that email — pending invites surface in the inbox via
  `ListPendingInvitationsForEmail`).

Both paths use a 7-day expiry. Tokens are sha256-hashed at rest;
`token_hash` is the column we look up by.

## Principals trigger

Two AFTER triggers (`tg_principals_user_sync`, `tg_principals_org_sync`)
maintain `principals` on every users/orgs INSERT/UPDATE/DELETE. The
slug PK on `principals` enforces global uniqueness across both tables
— a slug collision either with another user or another org is
rejected with SQLSTATE 23505, which the create path translates to
`ErrSlugTaken`.

Soft-deleted users/orgs are dropped from `principals` so their slug
becomes available — the username_redirects table still preserves the
old slug for 301s during the rename cooldown.

## What we deferred from the spec

* **`username_redirects` rename to `principal_redirects`**. The
  rename + `kind` column would cascade through every sqlc bundle
  (each one gets a regenerated model). Org renames aren't in the
  S30 DoD; deferred to a follow-up sprint that owns the rename
  refactor end to end.
* **Org-level audit log surface**, **suspension UI**, **org settings
  page**, **avatar upload**, **email notifications for role-change /
  remove / suspension / deletion**. Schema columns are present; UI and
  notification fan-out land in follow-ups.
* **Org renaming via `principal_redirects`** — depends on the
  rename refactor.
* **Daily digest / billing / SAML** — post-MVP per spec.

## Pitfalls noted in code

* **Slug-vs-username collision**: enforced by `principals` PK,
  tested by `TestCreate_RejectsCollisionWithUsername`.
* **Last-owner orphaning**: tested by
  `TestChangeRole_LastOwnerProtection`.
* **Email-invite claim by wrong user**: tested by
  `TestInvite_AcceptByEmailRejectsWrongUser` — only an account
  that owns the verified email matching the invite's `target_email`
  can claim.
* **Duplicate invitations**: idempotency check returns
  `ErrInvitationDuplicate` rather than minting a new token, so
  re-clicking Invite doesn't spam the recipient.
* **Reserved slugs**: `auth.IsReserved` filter applies to org
  slugs the same way it applies to usernames.
