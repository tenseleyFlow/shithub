// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

import "context"

// RepoPermissions is the gh-compat permission bundle surfaced on the
// raw repo response (audit-I11). gh's API emits these five booleans
// keyed off the requesting user's effective role on the repo so
// clients can light up or dim UI affordances without round-tripping
// the policy decision for each verb.
//
// shithub's role model has three levels (read / write / admin) where
// gh has five. We map:
//
//	gh.admin    ← shithub admin
//	gh.maintain ← shithub repo:settings:general (admin-ish, can edit settings)
//	gh.push     ← shithub repo:write
//	gh.triage   ← shithub repo:write (no distinct triage role today)
//	gh.pull     ← shithub repo:read
//
// Triage = Push is a deliberate over-approximation. gh's triage role
// is roughly "can manage issues/PRs but not push code"; without a
// separate row, anyone with write effectively has triage. Tightening
// this requires a new role tier.
type RepoPermissions struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Triage   bool `json:"triage"`
	Pull     bool `json:"pull"`
}

// AuthorAssociation is the gh-compat enum that surfaces on issue +
// PR + comment responses to describe how the author relates to the
// repo. The five values mirror gh's documented surface:
//
//	OWNER         — the author is the repo's owner (user-owned repo)
//	MEMBER        — the author is a member of the owning org
//	COLLABORATOR  — the author has an explicit collaborator row
//	CONTRIBUTOR   — the author has previously merged a PR (degrades to
//	                NONE on shithub today; we don't track historical
//	                contribution at issue-render time)
//	NONE          — no association
//
// Per-comment authors (in PR/issue threads) pass through the same
// helper; the policy decision is per-actor-per-repo, not per-resource.
//
// I7a (audit-I12): this lands as a CRIT-class field because porting
// gh scripts that switch on author_association (e.g. permissions
// matrices in bot-driven workflows) crash on the missing key.
func AuthorAssociation(ctx context.Context, d Deps, actor Actor, repo RepoRef) string {
	if actor.IsAnonymous || actor.UserID == 0 {
		return "NONE"
	}
	if repo.IsOwnedByUser(actor.UserID) {
		return "OWNER"
	}
	role := EffectiveRole(ctx, d, actor, repo)
	switch role {
	case RoleAdmin, RoleMaintain:
		// Org-owned repos: admin-via-org-membership surfaces as MEMBER.
		// User-owned repos: admin-via-collaborator-row → COLLABORATOR.
		if repo.OwnerOrgID != 0 {
			return "MEMBER"
		}
		return "COLLABORATOR"
	case RoleWrite, RoleTriage, RoleRead:
		// gh distinguishes COLLABORATOR (any explicit row) from
		// CONTRIBUTOR (merged-PR history). Without a historical
		// contribution table, every collaborator row surfaces here.
		return "COLLABORATOR"
	}
	return "NONE"
}

// PermissionsFor returns the gh-compat permission bundle for actor
// against repo. Pure projection over policy.Can — no DB writes, no
// new caching layer. Callers eat five Can calls per invocation; in
// practice the per-actor cache inside Can collapses these to one DB
// hit per request.
//
// Returns the all-false bundle (the zero value) for an anonymous
// caller on a private repo. Public-repo reads grant Pull but nothing
// else, mirroring gh's behavior.
func PermissionsFor(ctx context.Context, d Deps, actor Actor, repo RepoRef) RepoPermissions {
	return RepoPermissions{
		Admin:    Can(ctx, d, actor, ActionRepoAdmin, repo).Allow,
		Maintain: Can(ctx, d, actor, ActionRepoSettingsGeneral, repo).Allow,
		Push:     Can(ctx, d, actor, ActionRepoWrite, repo).Allow,
		Triage:   Can(ctx, d, actor, ActionRepoWrite, repo).Allow,
		Pull:     Can(ctx, d, actor, ActionRepoRead, repo).Allow,
	}
}
