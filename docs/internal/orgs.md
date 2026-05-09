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
GET  /{org}/people                 members + (owner-only) invite form
POST /{org}/people/invite          invite by username OR email
POST /{org}/people/{userID}/role   change role (owner-only)
POST /{org}/people/{userID}/remove remove member (owner-only)
GET  /invitations/{token}          accept/decline view (auth required)
POST /invitations/{token}/accept
POST /invitations/{token}/decline
```

`/{slug}` resolution flow inside `internal/web/handlers/profile/profile.go`:

1. Reserved-name check (defense in depth — chi already matches static
   routes first).
2. `orgs.Resolve(slug)` against `principals`. On `kind='org'`, dispatch
   to `serveOrgProfile`. On `kind='user'`, fall through to the
   existing user path. On miss, fall through to `username_redirects`
   for renamed users.

## Member roles

* `owner` → implicit `admin` on every org-owned repo (policy hook
  forthcoming).
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
* **Owner-implicit-admin in policy.Can**. The `org_members.role`
  shape is in place; wiring it into `policy.Can` so org owners
  automatically get `admin` on org-owned repos lands when the
  policy refactor next touches the repo-permission resolver.
* **Repo creation owner picker**. Repo create form still defaults
  to user-owner; extending the picker to list orgs the viewer is a
  member of (and honoring `allow_member_repo_create`) is one
  follow-up handler change.
* **Org-level audit log surface**, **suspension UI**, **soft-delete
  + grace + `org:hard_delete` worker**, **org settings page**,
  **avatar upload**, **email notifications for invite / role-change
  / remove**. Schema columns are present; UI + worker land in
  follow-ups.
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
