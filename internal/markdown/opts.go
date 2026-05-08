// SPDX-License-Identifier: AGPL-3.0-or-later

package markdown

import "context"

// RepoContext is the optional repo binding for `#N` reference
// resolution. When nil, `#N` renders as plain text (matches
// "repo-context-less rendering" in the spec).
type RepoContext struct {
	OwnerUsername string
	RepoName      string
	RepoID        int64
}

// Resolvers wire the package against the rest of the runtime without
// importing usersdb / issuesdb directly (avoids a cycle with packages
// that themselves render markdown). Each resolver is optional; a nil
// resolver means "render as plain text" for that flavor of reference.
//
// All resolvers MUST be visibility-aware: they receive the viewer
// (Options.ViewerUserID) and return ok=false when the resource isn't
// visible. Returning a link to a hidden resource leaks existence.
type Resolvers struct {
	// User: @username → "/username" if the user exists, isn't
	// suspended, and (post-S30) is org-team-visible to the viewer.
	User func(ctx context.Context, username string) (href string, ok bool)
	// Issue: (repoOwner, repoName, number) → "/owner/repo/issues/N".
	// Returns ok=false when the repo isn't visible to viewer or the
	// number wasn't allocated yet.
	Issue func(ctx context.Context, owner, name string, number int64, viewerUserID int64) (href string, ok bool)
	// Commit: short-or-full SHA inside the current repo context →
	// "/owner/repo/commit/<full_sha>". Only invoked when
	// Opts.Repo != nil.
	Commit func(ctx context.Context, repoOwner, repoName, shaPrefix string) (href, fullSHA string, ok bool)
}

// Options tunes a single Render call. Zero-value Options is valid
// and yields a safe, generic render with no reference resolution.
type Options struct {
	// Repo is the binding for same-repo `#N` resolution. nil means
	// "no repo context"; `#N` renders as plain text.
	Repo *RepoContext

	// ViewerUserID gates cross-repo `#N` and `@user` resolution.
	// 0 = anonymous viewer.
	ViewerUserID int64

	// SoftBreakAsBR controls newline-as-<br> rendering:
	//   true  — comment / issue / PR body shape (matches GitHub UI)
	//   false — README / structured-doc shape (preserve markdown semantics)
	SoftBreakAsBR bool

	// LinkTargetBlank, when true, sets target="_blank" rel="noopener
	// noreferrer" on autolinks + reference links. Default off; READMEs
	// keep links in-page so the user doesn't lose context.
	LinkTargetBlank bool

	// Resolvers wires reference resolution. nil resolvers in the
	// struct mean "render that flavor as plain text".
	Resolvers Resolvers
}
