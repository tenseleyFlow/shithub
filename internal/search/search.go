// SPDX-License-Identifier: AGPL-3.0-or-later

// Package search owns S28's search surface. Postgres FTS
// (`tsvector` + `pg_trgm`) backs everything; no external search
// engine. Visibility scoping flows through `policy.VisibilityPredicate`
// so every query is gated by the same rule the rest of the runtime
// uses.
//
// Entry points are:
//
//	SearchRepos / SearchIssues / SearchUsers / SearchCode — per-type
//	    queries returning slices of result rows + the total count.
//	ParseQuery — splits a user query string into the FTS query plus
//	    operator filters (repo:, is:, author:, state:).
package search

import (
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Deps wires the package against the rest of the runtime.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// Errors surfaced to handlers.
var (
	ErrEmptyQuery = errors.New("search: query is empty")
)

// PageSize is the per-type result count for the full results page.
// Quick-dropdown uses a smaller cap (see QuickResultsLimit).
const PageSize = 20

// QuickResultsLimit is the per-type cap for the top-bar quick
// dropdown. Same shape as GitHub's: tight for keystroke speed.
const QuickResultsLimit = 5

// MaxQueryBytes caps incoming query strings. Tighter than the
// markdown 1 MiB ceiling because no legitimate search ever needs
// even 1 KiB.
const MaxQueryBytes = 256

// RepoResult is one row from SearchRepos.
type RepoResult struct {
	ID            int64
	OwnerUsername string
	Name          string
	Description   string
	Visibility    string
	StarCount     int64
	UpdatedAt     time.Time
	Rank          float64
}

// IssueResult is one row from SearchIssues.
type IssueResult struct {
	ID            int64
	RepoID        int64
	OwnerUsername string
	RepoName      string
	Number        int64
	Title         string
	State         string
	Kind          string // "issue" | "pr"
	AuthorName    string
	UpdatedAt     time.Time
	Rank          float64
}

// UserResult is one row from SearchUsers.
type UserResult struct {
	ID          int64
	Username    string
	DisplayName string
	Bio         string
	Rank        float64
}

// CodeResult is one row from SearchCode. Either Path or Content
// (or both) is populated depending on which subquery hit.
type CodeResult struct {
	RepoID        int64
	OwnerUsername string
	RepoName      string
	RefName       string
	Path          string
	// PreviewLine is a single line of content extracted near the
	// match, when content matched. Empty for path-only hits.
	PreviewLine string
	Rank        float64
}
