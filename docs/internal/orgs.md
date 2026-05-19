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
GET  /organizations/plan           plan picker (auth required)
GET  /organizations/new            create form (auth required)
POST /organizations                create submit
GET  /{slug}                       /{user-or-org} — dispatched via principals.Resolve
POST /{slug}/pins                  owner-only org profile pin customization
GET  /orgs/{org}/repositories      repository list with filters + pagination
GET  /{org}/people                 members + (owner-only) invite form
POST /{org}/people/invite          invite by username OR email
POST /{org}/people/{userID}/role   change role (owner-only)
POST /{org}/people/{userID}/remove remove member (owner-only)
GET  /organizations/{org}/settings/profile
POST /organizations/{org}/settings/profile
POST /organizations/{org}/settings/profile/avatar
POST /organizations/{org}/settings/profile/avatar/remove
POST /organizations/{org}/settings/delete
GET  /organizations/{org}/settings/import
POST /organizations/{org}/settings/import
GET  /organizations/{org}/imports/{importID}
GET  /organizations/{org}/settings/scheduled-reminders
POST /organizations/{org}/settings/scheduled-reminders
POST /organizations/{org}/settings/scheduled-reminders/{reminderID}
POST /organizations/{org}/settings/scheduled-reminders/{reminderID}/pause
POST /organizations/{org}/settings/scheduled-reminders/{reminderID}/resume
POST /organizations/{org}/settings/scheduled-reminders/{reminderID}/delete
GET  /organizations/{org}/settings/billing
GET  /organizations/{org}/settings/billing/licensing
GET  /organizations/{org}/settings/billing/seats/add
POST /organizations/{org}/settings/billing/seats/add
GET  /organizations/{org}/settings/billing/seats/remove
POST /organizations/{org}/settings/billing/seats/remove
POST /organizations/{org}/billing/checkout
POST /organizations/{org}/billing/portal
GET  /organizations/{org}/billing/success
GET  /organizations/{org}/billing/cancel
GET  /invitations                  logged-in pending org invitation inbox
POST /invitations/id/{id}/accept   accept from the inbox / org people callout
POST /invitations/id/{id}/decline  decline from the inbox / org people callout
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

`GET /organizations/{org}/settings/profile` renders the owner-only
organization settings profile page. The page uses the GitHub settings
shape: org pagehead + underline nav, left settings sidebar, General
profile form, profile-picture aside, in-product message rows, and a
Danger zone. `POST /organizations/{org}/settings/profile` updates the
persisted org fields (`display_name`, `description`, `website`,
`location`, `billing_email`, and `allow_member_repo_create`) with
friendly length, URL, and email validation before writing through the
org sqlc queries. Avatar upload/removal stores object keys through the
avatar pipeline and object store.
`POST /organizations/{org}/settings/delete` soft-deletes the org through
`orgs.SoftDelete` after an owner confirms the slug; the hard-delete
worker still owns permanent removal after the grace window.

`GET /organizations/{org}/settings/import` renders the owner-only
GitHub organization import page. Owners can provide a GitHub
organization name or `github.com/<org>` URL and, optionally, a token.
Untokened imports discover public repositories only. Token-backed
imports use GitHub's `type=all` repo listing, persist the token
encrypted with the server secret box, and keep private upstream repos
private on shithub.

`POST /organizations/{org}/settings/import` creates an
`org_github_imports` row and enqueues
`worker.KindOrgGitHubImportDiscover`. `GET
/organizations/{org}/imports/{importID}` shows a polling progress page
with per-repo queued/importing/imported/skipped/failed state. The
discover job pages through the GitHub API, records one
`org_github_import_repos` row per repository, and enqueues an import job
per repo. Each repo job creates the org-owned shithub repository, saves
the GitHub source remote, fetches heads/tags with a temporary Git
askpass helper when a token is present, updates the default branch from
fetched refs, and enqueues indexing + size recalculation. Existing
active repositories in the organization are skipped instead of
overwritten.

`GET /organizations/{org}/settings/scheduled-reminders` renders the
owner-only scheduled reminders settings page for PR review follow-up.
Owners can create, update, pause, resume, and delete schedules that
target all repositories, one repository, or one team. Schedules store a
cron expression, timezone, reminder conditions for direct and team
review requests, and a minimum review-request age. Public repository
reminders are available to Free organizations. Any target that includes
private organization repositories requires the Team
`scheduled_reminders` entitlement; downgraded schedules remain stored
but writes that expand private reminder coverage are denied until the
org returns to Team.

Repo visibility is filtered through `policy.IsVisibleTo` using an actor
constructed from `middleware.CurrentUser`, including suspension,
site-admin, and impersonation write-mode fields. Anonymous viewers only
see public repositories; members and owners see whatever the policy
layer grants them.

`GET /orgs/{org}/repositories` is the dedicated organization
repositories surface, matching GitHub's current org route shape. It
uses the same policy-filtered visible repo set as the overview, then
applies query, type, language, sort, and page parameters in the handler.
The page renders 30 repositories per page with bordered GitHub-style
rows, topics, language/license/star/fork metadata, default-branch
activity sparklines, and numbered pagination.

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

* By **username**: orchestrator normalizes an optional leading `@`,
  resolves the username → user_id, and checks for existing membership
  before issuing the invite.
* By **email**: stores the email; recipient claims by signing in with
  any account that owns the verified email (or by signing up later
  with that email — pending invites surface in the inbox via
  `ListPendingInvitationsForEmail`).

Both paths use a 7-day expiry. Tokens are sha256-hashed at rest;
`token_hash` is the column we look up by for emailed links. Logged-in
users can also accept or decline any pending invitation addressed to
their user id or verified email from `GET /invitations`, and matching
org people pages render the same action callout.

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

## Billing posture

Organizations are the first paid shithub surface. `orgs.plan` remains
the human-facing summary, but product behavior goes through
`internal/entitlements` and the billing projection in
`org_billing_states`; production handlers must not branch directly on
`orgs.plan` for paid feature access.

`/organizations/plan` is the canonical Free / Team / Enterprise plan
picker. Existing "New organization" links route there. The picker uses
GitHub-style Free / Team / Enterprise cards, a seat-count advisor, and
a compare table whose rows carry owning sprint metadata and shipped /
planned state. Free membership is supported but is not marketed as an
unlimited-member headline; the Team pitch focuses on licensed seats for
private organization collaboration and controls. Team setup defaults to
5 licensed seats, accepts `seat_count` from the picker, shows `$4 x N`
before payment, and accepts GitHub's `plan=business` alias as Team.

When Stripe Billing is not fully configured, Team remains visible but
disabled; site admins see operator setup copy. Choosing Team creates
the organization first and then redirects the owner to hosted Stripe
Checkout; webhook processing, not checkout creation, activates paid
entitlements. Stripe success and cancel returns render explicit
post-checkout pages: success explains that Team activation waits for
webhook processing, and cancel leaves the organization on Free with a
path to retry checkout.
Existing owner-managed orgs also link to that billing page from
`/settings/organizations` for plan comparison.

Billing routes are mounted only when Stripe Billing is configured.
When billing is disabled, local billing rows remain in the database but
paid onboarding links stay unavailable. The durable product and
implementation contract is tracked in [`billing.md`](./billing.md).

Team seat management lives in shithub, not in Stripe Portal:
`/settings/billing/licensing` shows licensed, used, and available
seats plus the member list consuming those seats. Owners add or remove
licensed seats through explicit confirmation pages that update Stripe
subscription quantity before changing local license state. Team member
invitations redirect back to People with an Add seats action when the
organization has no available seats.

## What we deferred from the spec

* **`username_redirects` rename to `principal_redirects`**. The
  rename + `kind` column would cascade through every sqlc bundle
  (each one gets a regenerated model). Org renames aren't in the
  S30 DoD; deferred to a follow-up sprint that owns the rename
  refactor end to end.
* **Org-level audit log surface**, **suspension UI**, **org rename /
  archive settings actions**, **email notifications for role-change /
  remove / suspension / deletion**. Schema columns are present; UI and
  notification fan-out land in follow-ups.
* **Org renaming via `principal_redirects`** — depends on the
  rename refactor.
* **Daily digest / SAML** — post-MVP per spec.

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
* **Org import token storage**: the token never enters a Git URL, logs,
  or plaintext database column. It is encrypted in
  `org_github_imports.token_ciphertext` and supplied to git via a
  temporary askpass script scoped to the fetch process.
