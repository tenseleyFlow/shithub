// SPDX-License-Identifier: AGPL-3.0-or-later

// Package policy is the single source of truth for "who can do what".
// Every authorization decision in shithub flows through Can(); handlers
// must not read ownership, visibility, or collaborator state inline.
//
// The package shape:
//
//	Actor      — who is asking (anonymous, suspended, site-admin etc.)
//	Resource   — what they want to act on (a RepoRef today; org/team later)
//	Action     — a constant from the registry below
//	Can(...)   — the only public decision function
//	Decision   — { Allow bool; Reason string }
//
// Reasons are for logs and admin debugging, never for end-user error
// strings — they can leak existence.
package policy

// Action is a constant identifier for an operation against some resource.
// Adding a new action: add the const, register a default rule in
// policy.Can, and extend the test matrix. The matrix test asserts that
// every Action has explicit coverage for every Actor archetype.
type Action string

// Repo-level actions.
const (
	ActionRepoRead  Action = "repo:read"
	ActionRepoWrite Action = "repo:write"
	ActionRepoAdmin Action = "repo:admin"

	ActionRepoSettingsGeneral       Action = "repo:settings:general"
	ActionRepoSettingsCollaborators Action = "repo:settings:collaborators"
	ActionRepoSettingsBranches      Action = "repo:settings:branches"

	ActionRepoArchive    Action = "repo:archive"
	ActionRepoDelete     Action = "repo:delete"
	ActionRepoTransfer   Action = "repo:transfer"
	ActionRepoVisibility Action = "repo:visibility"
)

// Issue-level actions. (Issue resources arrive in S18; S15 just ships
// the registry entries so the matrix is exhaustive from day one.)
const (
	ActionIssueRead    Action = "issue:read"
	ActionIssueCreate  Action = "issue:create"
	ActionIssueComment Action = "issue:comment"
	ActionIssueClose   Action = "issue:close"
	ActionIssueLabel   Action = "issue:label"
	ActionIssueAssign  Action = "issue:assign"
)

// Pull-request actions. (Pull resources arrive in S19; same note as above.)
const (
	ActionPullRead   Action = "pull:read"
	ActionPullCreate Action = "pull:create"
	ActionPullMerge  Action = "pull:merge"
	ActionPullReview Action = "pull:review"
	ActionPullClose  Action = "pull:close"
)

// Per-user social actions.
const (
	ActionStarCreate Action = "star:create"
	ActionForkCreate Action = "fork:create"

	// S26 social actions. WatchSet covers both setting an explicit
	// level and unsetting (deleting the row). Both require a logged-in
	// user with read access to the repo — the policy.Can engine
	// enforces visibility before reaching the role check, so a
	// non-collab on a private repo deny-leaks as 404.
	ActionWatchSet Action = "watch:set"
)

// AllActions is the canonical list. The matrix test iterates this so a
// new Action that's not registered above will fail coverage and force
// the author to think through every actor archetype.
var AllActions = []Action{
	ActionRepoRead, ActionRepoWrite, ActionRepoAdmin,
	ActionRepoSettingsGeneral, ActionRepoSettingsCollaborators, ActionRepoSettingsBranches,
	ActionRepoArchive, ActionRepoDelete, ActionRepoTransfer, ActionRepoVisibility,
	ActionIssueRead, ActionIssueCreate, ActionIssueComment, ActionIssueClose, ActionIssueLabel, ActionIssueAssign,
	ActionPullRead, ActionPullCreate, ActionPullMerge, ActionPullReview, ActionPullClose,
	ActionStarCreate, ActionForkCreate,
	ActionWatchSet,
}

// isWriteAction returns true when the action mutates state. Used by the
// suspended-user gate (suspended accounts can read but not write).
func isWriteAction(a Action) bool {
	switch a {
	case ActionRepoRead, ActionIssueRead, ActionPullRead:
		return false
	default:
		return true
	}
}

// isReadAction is the inverse, broken out for readability at call sites
// that branch on intent rather than on the absence of writes.
func isReadAction(a Action) bool { return !isWriteAction(a) }
