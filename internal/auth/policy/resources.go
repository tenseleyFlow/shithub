// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

// RepoRef is the policy-side projection of a repos row. Construct from
// either reposdb.Repo or any of the GetRepoBy* row types via NewRepoRef
// helpers in the policy package's adapters file (kept policy-internal
// so the policy package never imports reposdb directly — sqlc rows
// flow in via constructors at call boundaries).
//
// OwnerOrgID is the S31-shape: zero today; populated when org-owned
// repos ship. Can() already branches on OwnerOrgID > 0 so that S31's
// org membership lookup plugs in cleanly.
type RepoRef struct {
	ID          int64
	OwnerUserID int64
	OwnerOrgID  int64
	Visibility  string // "public" | "private"
	IsArchived  bool
	IsDeleted   bool
}

// IsPublic returns true when the repo's visibility column is "public".
// Other code paths must not parse Visibility directly — read it through
// these helpers so the canonical strings live in one place.
func (r RepoRef) IsPublic() bool { return r.Visibility == "public" }

// IsPrivate is the inverse. Use whichever phrasing reads better at the
// call site.
func (r RepoRef) IsPrivate() bool { return r.Visibility == "private" }
