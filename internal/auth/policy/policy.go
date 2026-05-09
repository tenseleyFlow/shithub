// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	policydb "github.com/tenseleyFlow/shithub/internal/auth/policy/sqlc"
)

// Deps wires the policy package against Postgres. Construct once at
// boot and keep alive for the process lifetime.
type Deps struct {
	Pool *pgxpool.Pool
}

// DenyCode is a typed enum carried on a deny Decision so handlers can
// pick a friendly user-facing message without re-deriving the reason
// from the resource. Read this; do not parse Decision.Reason.
type DenyCode int

const (
	// DenyNone is the zero value used on an allow Decision.
	DenyNone DenyCode = iota
	DenyRepoDeleted
	DenyActorSuspended
	DenyArchived
	DenyVisibility // anonymous-or-non-collab on a private repo
	DenyRoleTooLow // logged-in but role insufficient
	DenyAnonymous  // login required (e.g. star/fork)
	DenyDBError
	// DenyOrgSuspended is returned for write actions on a repo whose
	// owning org is currently suspended. Reads stay allowed (the spec
	// preserves visibility into suspended-org content); writes flip
	// off uniformly.
	DenyOrgSuspended
	// DenyImpersonationReadOnly is returned when an admin in
	// impersonation mode attempts a write without first opting into
	// write-mode (the typed-name confirm step in /admin/impersonate).
	// The default is read-only on the canonical foot-gun grounds.
	DenyImpersonationReadOnly
)

// Decision is the verdict from Can. Allow is the only field handlers
// should branch on for control flow. DenyCode lets the handler pick a
// user-facing message; Reason is for logs and tests, never end-user
// surfaces — it can carry implementation details that constitute
// existence leaks.
type Decision struct {
	Allow  bool
	Reason string
	Code   DenyCode
}

// allow / deny are convenience constructors used by the rule engine
// below to keep each branch one short line.
func allow(reason string) Decision { return Decision{Allow: true, Reason: reason} }

func deny(code DenyCode, reason string) Decision {
	return Decision{Allow: false, Reason: reason, Code: code}
}

// Can is the single authorization decision function. Every handler,
// hook, and worker in shithub funnels through here. Order of evaluation
// is significant — higher-precedence denies (deletion, suspension)
// short-circuit before role lookups touch the DB.
//
// The cache (if present in ctx via WithCache) is consulted before any
// query and populated on the first lookup.
func Can(ctx context.Context, d Deps, actor Actor, action Action, repo RepoRef) Decision {
	// 1. Soft-deleted repos: nothing is allowed against them, full stop.
	//    (Site admins can technically still hard-delete via admin
	//    tooling, which doesn't go through Can.)
	if repo.IsDeleted {
		return deny(DenyRepoDeleted, "repo deleted")
	}

	// 2. Site admins: read access to anything; explicit-impersonation
	//    needed for writes (S34 wires the impersonation surface — for
	//    now the override only fires on read actions).
	if actor.IsSiteAdmin && isReadAction(action) {
		return allow("site admin read")
	}

	// 3. Suspended actors: writes denied unconditionally; reads against
	//    public repos are still allowed (matches GitHub's "ghost-mode"
	//    suspension semantics).
	if actor.IsSuspended && isWriteAction(action) {
		return deny(DenyActorSuspended, "actor suspended")
	}

	// 3a. Impersonation: an admin viewing-as another user is read-only
	//     by default. Writes require ImpersonateWriteOK to have been
	//     opted into via the typed-name confirmation step.
	if actor.Impersonating && !actor.ImpersonateWriteOK && isWriteAction(action) {
		return deny(DenyImpersonationReadOnly, "impersonation in read-only mode")
	}

	// 4. Anonymous + private: existence-leak-safe deny. Handler maps to
	//    404 via Maybe404.
	if actor.IsAnonymous && repo.IsPrivate() {
		return deny(DenyVisibility, "anonymous on private repo")
	}

	// 5. Visibility for reads on public repos: anyone (anon or logged-
	//    in) can read public, regardless of suspension.
	if isReadAction(action) && repo.IsPublic() {
		return allow("public repo read")
	}

	// 6. Public issue participation: any logged-in user can open or
	//    comment on issues in a public repo. Private repos still fall
	//    through to the role check below, where read access is required.
	//    Archive/org-suspension write gates stay below role resolution
	//    for the general case, so enforce them explicitly here before
	//    allowing a non-collaborator public-repo issue action.
	if isIssueParticipationAction(action) && repo.IsPublic() {
		if actor.IsAnonymous {
			return deny(DenyAnonymous, "anonymous cannot create/comment on issues")
		}
		if repo.IsArchived {
			return deny(DenyArchived, "repo archived")
		}
		if repo.OwnerOrgID != 0 && isOrgSuspended(ctx, d, repo.OwnerOrgID) {
			return deny(DenyOrgSuspended, "owning org suspended")
		}
		return allow("public issue participation")
	}

	// 7. From here we need the actor's effective role on the repo.
	//    Owner short-circuits to admin; collaborator role from DB
	//    otherwise; org membership stub for S31.
	role, err := effectiveRole(ctx, d, actor, repo)
	if err != nil {
		// DB error: deny rather than allow. Surface in logs via the
		// reason; the caller is expected to log and fail closed.
		return deny(DenyDBError, "role lookup failed: "+err.Error())
	}

	// 7a. Author-self-close on issues and PRs. The author of an issue or
	//     PR is allowed to close (and reopen — same Action) their own
	//     thread regardless of their collaborator role. Handlers populate
	//     `repo.AuthorUserID` on the close path; everywhere else the
	//     field is zero and this branch is dead. Suspension and archived
	//     gates above still apply — they ran before this. Note: we do NOT
	//     extend this to `ActionIssueLabel`/`ActionIssueAssign`; only the
	//     close action is author-self by design (matches GitHub).
	if (action == ActionIssueClose || action == ActionPullClose) &&
		repo.AuthorUserID != 0 && repo.AuthorUserID == actor.UserID {
		return allow("author of thread")
	}

	// 8. Archived repos: writes denied even for owners. Reads still go
	//    through the role check above. (We could short-circuit reads
	//    earlier but keeping the flow uniform makes the matrix readable.)
	if repo.IsArchived && isWriteAction(action) {
		return deny(DenyArchived, "repo archived")
	}

	// 8b. Org suspension (S30): writes against any repo owned by a
	//     suspended org are denied uniformly. Reads stay allowed (the
	//     org's contributions to the broader graph aren't erased).
	//     The check is gated on a write action AND a non-zero
	//     OwnerOrgID so user-owned repos pay nothing for it.
	if repo.OwnerOrgID != 0 && isWriteAction(action) && isOrgSuspended(ctx, d, repo.OwnerOrgID) {
		return deny(DenyOrgSuspended, "owning org suspended")
	}

	// 9. Map action → minimum required role; check.
	want := minRoleFor(action)
	if want != RoleNone && !RoleAtLeast(role, want) {
		// No role at all + private repo → look like a visibility deny
		// so the handler picks the 404-leak guard. Otherwise it's a
		// genuine role-too-low for a logged-in collaborator.
		if role == RoleNone && repo.IsPrivate() {
			return deny(DenyVisibility, "no role on private repo")
		}
		return deny(DenyRoleTooLow, "role too low")
	}

	// 10. Login-required actions: star/fork/watch-set need any
	//    logged-in user. Anonymous reaches here only on a public repo
	//    (see step 4); we deny with the anonymous code so the handler
	//    can render a friendly "log in to star" prompt.
	if (action == ActionStarCreate || action == ActionForkCreate || action == ActionWatchSet) &&
		actor.IsAnonymous {
		return deny(DenyAnonymous, "anonymous cannot star/fork/watch")
	}

	return allow("granted")
}

func isOrgSuspended(ctx context.Context, d Deps, orgID int64) bool {
	if d.Pool == nil {
		return false
	}
	var suspended bool
	err := d.Pool.QueryRow(
		ctx,
		`SELECT suspended_at IS NOT NULL FROM orgs WHERE id = $1`,
		orgID,
	).Scan(&suspended)
	return err == nil && suspended
}

// IsVisibleTo is a convenience wrapper around Can(actor, repo:read, …).
// Used by listing endpoints that need to filter results by visibility
// without caring about the deny reason.
func IsVisibleTo(ctx context.Context, d Deps, actor Actor, repo RepoRef) bool {
	return Can(ctx, d, actor, ActionRepoRead, repo).Allow
}

// Maybe404 maps a deny Decision to an HTTP status code that doesn't
// leak existence. The convention:
//
//   - allow → 200 (caller handles)
//   - deny on a private+anonymous (or non-collab+non-owner) → 404
//   - any other deny → 403
//
// Handlers should call this after checking Allow, only when serving
// the rejection response.
func Maybe404(decision Decision, repo RepoRef, actor Actor) int {
	if decision.Allow {
		return http.StatusOK
	}
	// Anything that turns on private-visibility surfaces as 404.
	if repo.IsPrivate() {
		// Owner of a private repo getting denied is a real 403 (e.g.
		// archived push). We approximate "owner" as having matching
		// owner_user_id; collaborator status doesn't change the leak
		// analysis since the row already exists in their world.
		if actor.UserID != 0 && actor.UserID == repo.OwnerUserID {
			return http.StatusForbidden
		}
		return http.StatusNotFound
	}
	return http.StatusForbidden
}

// effectiveRole computes the highest-effective role for actor on repo.
// Owner ⇒ implicit admin; collaborator role from repo_collaborators;
// nothing otherwise.
//
// Org-owned repos: every `org_members.role='owner'` of the owning org
// is treated as an implicit admin on every org-owned repo. This is
// the S30 owner-implicit-admin contract — without it an org owner
// can't push to their own org's repos. Org `member` role grants no
// implicit access; teams (S31) and direct collaboration (S15) are the
// only paths to repo permission for non-owners.
func effectiveRole(ctx context.Context, d Deps, actor Actor, repo RepoRef) (Role, error) {
	if actor.UserID != 0 && actor.UserID == repo.OwnerUserID {
		return RoleAdmin, nil
	}
	if actor.UserID == 0 {
		return RoleNone, nil
	}

	// Cache check.
	cache := cacheFromContext(ctx)
	key := cacheKey{actorUserID: actor.UserID, repoID: repo.ID}
	if r, ok := cacheGet(cache, key); ok {
		return r, nil
	}

	if d.Pool == nil {
		// In tests where a Deps without Pool is passed, fail closed.
		return RoleNone, nil
	}

	// Org-owner check fires first because it short-circuits with admin
	// regardless of any per-repo collaborator row. The lookup is
	// indexed on (org_id, user_id) — same cost as the collab lookup.
	if repo.OwnerOrgID != 0 {
		var dbOrgRole string
		err := d.Pool.QueryRow(
			ctx,
			`SELECT role::text FROM org_members WHERE org_id = $1 AND user_id = $2`,
			repo.OwnerOrgID, actor.UserID,
		).Scan(&dbOrgRole)
		if err == nil && dbOrgRole == "owner" {
			cachePut(cache, key, RoleAdmin)
			return RoleAdmin, nil
		}
		// "no rows" or member-only falls through to the collab-row
		// lookup below.
	}

	// Effective role = MAX(direct collab, team grants). Team path
	// runs only for org-owned repos (user-owned repos have no
	// teams). One hop on parent_team_id captures the inherited
	// grants per S31's one-level-deep rule.
	best := RoleNone
	q := policydb.New()
	dbRole, err := q.GetCollabRole(ctx, d.Pool, policydb.GetCollabRoleParams{
		RepoID: repo.ID,
		UserID: actor.UserID,
	})
	switch {
	case err == nil:
		best = roleFromDB(dbRole)
	case errors.Is(err, pgx.ErrNoRows):
		// no direct collab row — best stays RoleNone
	default:
		return RoleNone, err
	}
	if repo.OwnerOrgID != 0 {
		teamRole, terr := teamGrantedRole(ctx, d, actor.UserID, repo.OwnerOrgID, repo.ID)
		if terr != nil {
			return RoleNone, terr
		}
		// roleStronger picks the higher-rank role. Don't use
		// RoleAtLeast here — its `want > 0` guard treats RoleNone
		// as un-comparable and blocks the legitimate "any role
		// beats no role" branch.
		if roleStronger(teamRole, best) {
			best = teamRole
		}
	}
	cachePut(cache, key, best)
	return best, nil
}

// roleStronger reports whether `a` ranks strictly higher than `b`,
// where RoleNone is the bottom (rank 0). Used to compose role
// sources (direct collab, team grant, parent-team grant) into a
// single max — the spec's "effective role = max of all sources"
// rule.
func roleStronger(a, b Role) bool {
	return roleRank(a) > roleRank(b)
}

// teamGrantedRole computes the highest role the actor inherits from
// any team in the org that has a grant on the repo. Walks parent
// teams one hop (per the one-level-nesting cap from migration 0035).
func teamGrantedRole(ctx context.Context, d Deps, userID, orgID, repoID int64) (Role, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT t.id, t.parent_team_id
		   FROM team_members m
		   JOIN teams t ON t.id = m.team_id
		  WHERE t.org_id = $1 AND m.user_id = $2`,
		orgID, userID)
	if err != nil {
		return RoleNone, err
	}
	defer rows.Close()
	teamIDs := []int64{}
	for rows.Next() {
		var id int64
		var parent pgtype.Int8
		if err := rows.Scan(&id, &parent); err != nil {
			return RoleNone, err
		}
		teamIDs = append(teamIDs, id)
		if parent.Valid {
			teamIDs = append(teamIDs, parent.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return RoleNone, err
	}
	if len(teamIDs) == 0 {
		return RoleNone, nil
	}
	grantRows, err := d.Pool.Query(ctx,
		`SELECT role::text FROM team_repo_access
		  WHERE repo_id = $1 AND team_id = ANY($2::bigint[])`,
		repoID, teamIDs)
	if err != nil {
		return RoleNone, err
	}
	defer grantRows.Close()
	best := RoleNone
	for grantRows.Next() {
		var role string
		if err := grantRows.Scan(&role); err != nil {
			return RoleNone, err
		}
		if r := teamRepoRoleToPolicyRole(role); roleStronger(r, best) {
			best = r
		}
	}
	return best, grantRows.Err()
}

// teamRepoRoleToPolicyRole maps the team_repo_role enum string to
// the policy.Role string. Names align but the typed enums are in
// different packages so the conversion is explicit.
func teamRepoRoleToPolicyRole(s string) Role {
	switch s {
	case "read":
		return RoleRead
	case "triage":
		return RoleTriage
	case "write":
		return RoleWrite
	case "maintain":
		return RoleMaintain
	case "admin":
		return RoleAdmin
	}
	return RoleNone
}

// minRoleFor returns the minimum collaborator role required for the
// action against an existing repo. Owner is implicit admin, so this
// table is the single source of truth for "what role grants what."
//
//nolint:gocyclo // exhaustive switch is the readable shape here.
func minRoleFor(action Action) Role {
	switch action {
	// Read tier — public repos bypass via the early allow in Can; this
	// is the gate for *private* read access.
	case ActionRepoRead, ActionIssueRead, ActionPullRead:
		return RoleRead

	// Triage tier — issue-shape mutations without code write.
	case ActionIssueClose, ActionIssueLabel, ActionIssueAssign:
		return RoleTriage

	// Write tier — code push, branch create, PR open/comment.
	case ActionRepoWrite, ActionPullCreate, ActionPullReview, ActionPullClose:
		return RoleWrite

	// Issue participation on private repos requires read access. Public
	// repos are handled by Can's public issue participation branch above.
	case ActionIssueCreate, ActionIssueComment:
		return RoleRead

	// Maintain tier — most settings except dangerous ones.
	case ActionRepoSettingsGeneral, ActionRepoSettingsBranches:
		return RoleMaintain

	// Admin tier — destructive and ownership-changing actions.
	case ActionRepoAdmin, ActionRepoSettingsCollaborators,
		ActionRepoArchive, ActionRepoDelete, ActionRepoTransfer, ActionRepoVisibility,
		ActionPullMerge:
		return RoleAdmin

	// Login-required but no role required (any logged-in user).
	// Star/fork: any logged-in user can star or fork any repo they
	// can read — the visibility short-circuit above already gates
	// private-repo access, and the role check below short-circuits
	// to allow when minRole is RoleNone.
	// WatchSet: same shape — any logged-in user with read access
	// can choose their watch level. The downstream notifications
	// fan-out (S29) is what enforces level-based delivery.
	case ActionStarCreate, ActionForkCreate, ActionWatchSet:
		return RoleNone

	default:
		// Unknown actions deny by default — RoleAdmin is impossibly
		// high for a stranger so this acts as a fail-closed gate.
		return RoleAdmin
	}
}

// EffectiveRole returns the actor's resolved role on repo, taking
// owner-equals-admin into account and consulting the per-request cache.
//
// Use this when a handler needs to ask "is this actor at least X" for a
// rule that doesn't map cleanly to an Action — for example, the
// locked-issue gate which lets triage+ comment despite the lock without
// granting them any other write permission.
//
// Returns RoleNone for anonymous actors and on any DB error; callers
// should treat RoleNone as "no permissions."
func EffectiveRole(ctx context.Context, d Deps, actor Actor, repo RepoRef) Role {
	if actor.IsAnonymous {
		return RoleNone
	}
	r, err := effectiveRole(ctx, d, actor, repo)
	if err != nil {
		return RoleNone
	}
	return r
}

// HasRoleAtLeast is the convenience shorthand for the common case:
// "does this actor hold at least `want` on this repo?" Wraps
// EffectiveRole + RoleAtLeast.
func HasRoleAtLeast(ctx context.Context, d Deps, actor Actor, repo RepoRef, want Role) bool {
	return RoleAtLeast(EffectiveRole(ctx, d, actor, repo), want)
}
