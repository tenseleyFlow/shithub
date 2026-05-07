// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

import policydb "github.com/tenseleyFlow/shithub/internal/auth/policy/sqlc"

// Role models the per-collaborator grant. The five values mirror
// GitHub's tiers; the order matters because RoleAtLeast depends on it.
type Role string

const (
	// RoleNone is the zero value used by callers when no row exists.
	RoleNone     Role = ""
	RoleRead     Role = "read"
	RoleTriage   Role = "triage"
	RoleWrite    Role = "write"
	RoleMaintain Role = "maintain"
	RoleAdmin    Role = "admin"
)

// roleRank maps a role to its position in the lattice. Higher rank →
// strictly more powerful. RoleNone is below everything; RoleAdmin is
// the top.
func roleRank(r Role) int {
	switch r {
	case RoleAdmin:
		return 5
	case RoleMaintain:
		return 4
	case RoleWrite:
		return 3
	case RoleTriage:
		return 2
	case RoleRead:
		return 1
	default:
		return 0
	}
}

// RoleAtLeast reports whether `have` grants at least `want`. RoleNone
// grants nothing.
func RoleAtLeast(have, want Role) bool {
	return roleRank(have) >= roleRank(want) && roleRank(want) > 0
}

// roleFromDB translates the sqlc-generated enum to our domain type.
// We mirror values rather than re-using the DB type so the policy
// package's API doesn't require callers to import policydb.
func roleFromDB(r policydb.CollabRole) Role {
	return Role(r)
}

// roleToDB is the inverse, used by callers that mutate collaborators.
func roleToDB(r Role) policydb.CollabRole {
	return policydb.CollabRole(r)
}
