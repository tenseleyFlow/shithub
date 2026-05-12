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

	// AuthorUserID is the author of the *issue or PR* being acted on,
	// when the action is one that grants author-self privileges
	// (`ActionIssueClose`, `ActionPullClose`). Zero means "no author
	// context" — e.g. a repo-level read or write — and is the default.
	// Handlers populate this only on the close paths; everywhere else
	// it stays zero.
	//
	// This is a pragmatic v1 shape: instead of introducing an
	// IssueRef/PullRef parallel to RepoRef, we widen RepoRef with the
	// one fact the close gate needs. When/if more issue-level rules
	// land (assignee privileges, labeler privileges) we'll graduate
	// to a proper IssueRef. The audit captured the design tradeoff;
	// don't add new fields here without re-reading that note.
	AuthorUserID int64
}

// IsPublic returns true when the repo's visibility column is "public".
// Other code paths must not parse Visibility directly — read it through
// these helpers so the canonical strings live in one place.
func (r RepoRef) IsPublic() bool { return r.Visibility == "public" }

// IsPrivate is the inverse. Use whichever phrasing reads better at the
// call site.
func (r RepoRef) IsPrivate() bool { return r.Visibility == "private" }

// IsOwnedByUser reports whether userID is the direct user owner. Keep
// ownership interpretation here so handlers do not grow inline owner
// checks next to request behavior.
func (r RepoRef) IsOwnedByUser(userID int64) bool {
	return userID != 0 && r.OwnerUserID == userID
}
