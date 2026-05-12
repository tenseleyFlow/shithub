# Organizations & teams

Organizations group people and repos under a shared namespace.
A repo at `acme/widget` is owned by the `acme` org, and access
is managed through teams + direct collaborators.

## Creating an org

Account menu → "New organization". You'll need:

- A name — same rules as usernames; cannot collide with an
  existing user or org.
- A primary email (for billing/notifications; org settings only).

When paid organization billing is enabled, the flow asks you to pick
Free or Team before the form. The creator is the first owner.

## Roles

- **Owner** — full admin: settings, billing, member management,
  any repo. There must always be at least one owner.
- **Member** — appears in the org's people list; sees public + the
  private repos a team grants them.

A user can be both an owner and a team member.

## Inviting members

Org → People → "Invite member". Type an existing username or
email. The invitee gets a notification + email; they accept on a
landing page and become a member immediately.

Invitations expire after 7 days; resend or revoke from the same
page.

## Removing members

Owner → click a member → "Remove". The user loses team
memberships and any direct grants the org had given them. If they
authored issues/PRs, those stay; if they had pending invitations,
those are cancelled.

## Teams

Teams are how you grant repo access at scale.

- **Create:** Org → Teams → "New team".
- **Members:** add org members; non-members must be invited to
  the org first.
- **Repo access:** add repos with one of `read`, `triage`, `write`,
  `maintain`, `admin`. Multiple teams can grant on the same repo;
  the user's effective permission is the **maximum**.
- **One-level nesting:** a team can have a parent team. Members
  of a parent team are also considered members of every child
  team for permission purposes.

## Mentioning a team

`@acme/security` in any markdown body notifies every member of
that team. Useful for review requests and triage.

## Org-owned repos

Indistinguishable from user-owned repos in most ways; the
difference is that access is governed by team grants instead of
the owner being a single user.

## Audit

Org → Settings → Audit log surfaces org-level events: member
add/remove, team create/delete, repo transferred in/out, role
changes. Same per-row IP + user-agent capture as the personal
audit log.
