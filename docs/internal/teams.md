# Teams (S31)

Teams are sub-groups within an organization. They're the canonical
way to grant repo access to a set of users without per-user collab
rows. The S15 policy aggregator unions team-granted permissions with
direct collaborator permissions and the org-owner implicit-admin rule.

## Schema (migration 0035)

```
teams              — (org_id, slug, parent_team_id, privacy, …)
team_members       — (team_id, user_id, role enum('member','maintainer'))
team_repo_access   — (team_id, repo_id, role enum('read'..'admin'))
```

**One-level nesting** is structurally enforced by a `BEFORE INSERT
OR UPDATE` trigger that checks the parent team's `parent_team_id IS
NULL`. Two-level deep nesting raises SQLSTATE 23514, which the
orchestrator translates to `ErrTeamNestingTooDeep`.

## Permission resolution

The S15 policy contract gets one new source. For an org-owned repo,
`policy.effectiveRole` returns:

```
max(
  org_role == owner   → admin,         (S30)
  direct collab role,                  (S15: repo_collaborators)
  team grant role over team-set,       (S31: team_repo_access)
)
```

Where the **team-set** for a user in an org is:
- every team they're a direct member of, **plus**
- the parent team of each (one hop, per nesting cap).

The aggregator resolves team-set + grants in two indexed queries
(`team_members JOIN teams` + `team_repo_access WHERE team_id = ANY(...)`)
and stores the winning role in the per-request `policy.WithCache`
memo so subsequent `Can()` calls in the same request reuse it.

`team_role = maintainer` is about managing the team (adding members);
it does **not** elevate repo perms beyond the team's
`team_repo_access.role`. Confusion with the repo `maintain` role is
flagged in the team-management UI.

## Visibility

* `privacy='visible'`: team metadata visible to all org members; basic
  info (name + description) visible to anyone who can see the org.
* `privacy='secret'`: team metadata, members, and repo grants visible
  only to (team members ∪ org owners).

The list page filters secret teams the viewer can't see; the team
view page 404s for the same reason. The markdown `@org/team` resolver
must return `ok=false` for secret teams the viewer can't see — it
falls back to plain text rather than leaking existence via a 404.

## Cross-repo refs + mentions

* **`owner/repo#N`**: now dispatches via `principals.Resolve` so
  org-owned repos resolve. `internal/issues/references.go` was the
  S21 deferral; cleared this sprint.
* **`@org/team`**: a new alternation in
  `internal/markdown/extensions/extensions.go::reCombined` matches
  the slash form before the bare `@user` branch, so the trailing
  `/team` doesn't get split off. Routes to `/{org}/teams/{team}`
  via the Team resolver. S25 deferral cleared.

## Team reviewers and CODEOWNERS

SP19 uses teams as first-class pull request reviewer targets. A review
request row may target either `requested_user_id` or
`requested_team_id`; submitting an `approve` or `request_changes`
review satisfies a team request when the reviewer is a current member
of that team.

CODEOWNERS supports `@org/team` owners for repositories owned by the
same organization. To resolve as a valid code owner, the team must have
explicit repository access with `write`, `maintain`, or `admin` role.
This keeps CODEOWNERS review requirements aligned with GitHub's
"owners must have write access" rule and avoids granting review power
from mention syntax alone.

## Routes

```
GET  /{org}/teams                         list (secret teams filtered)
POST /{org}/teams                         create  (owner-only)
GET  /{org}/teams/{slug}                  view: members + repo grants
POST /{org}/teams/{slug}/members          add (form + role) / remove (action=remove)
POST /{org}/teams/{slug}/repos            grant (repo_id + role) / revoke (action=remove)
```

## What we deferred from the spec

* **Set-parent UI**: orchestrator (`SetTeamParent`) + the trigger
  validation are in place; the form to retarget a parent in the UI
  lands in a follow-up.
* **Repo-settings team-grant UI**: the team-side grant form on
  `/{org}/teams/{slug}` works today; mirroring it on
  `/{owner}/{repo}/settings/access` is S32 territory.
* **Team rename + redirects**: `team_redirects` table from the spec
  isn't shipped; defer to the same follow-up that does the
  `username_redirects → principal_redirects` rename.
* **Notifications for "added to team" / "team granted access"**:
  routing matrix in S29 is wired for arbitrary kinds; emit-side hook
  lands when these surfaces next get touched.
* **API endpoints** `GET /api/v1/orgs/{org}/teams` etc.: same
  posture as S30 — defer to S33/S34 API consolidation.
* **SCIM team provisioning** is post-MVP per spec.

## Pitfalls noted in code

* **Permission caching staleness**: the per-request memo is
  invalidated by `policy.InvalidateRepo`; cross-request changes are
  picked up on the next request.
* **Cycle prevention**: `parent_team_id != id` row CHECK + the
  one-level trigger together prevent every cycle shape.
* **Deleting the parent team** flips children to top-level via
  `ON DELETE SET NULL` on `parent_team_id` (the spec's lean).
* **Outside collaborator on a member's account**: removing the user
  from the org clears team memberships but NOT direct collab rows
  — this matches GitHub. Implementation note: today the org member
  removal in `orgs.RemoveMember` doesn't delete team rows
  explicitly; the `org_members` row removal cascades through
  `team_members` only via shared `users` FK. **Follow-up**: when
  `orgs.RemoveMember` lands its policy-driven cleanup pass, it
  should also `DELETE FROM team_members WHERE user_id = $1 AND
  team_id IN (SELECT id FROM teams WHERE org_id = $2)`. Tracked.
* **Secret-team metadata leak via repo-settings page**: the
  `/{owner}/{repo}/settings/access` extension ships in S32; the
  visibility filter must match the team-list page's logic.
