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
