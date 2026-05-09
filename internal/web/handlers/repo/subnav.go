// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// repoSubnavData is the shape the `repo-subnav` partial reads. Counts
// are best-effort: a query failure collapses to zero so the nav still
// renders. The `CanSettings` flag is a UX hint — the actual permission
// check happens inside the settings handler via policy.Can.
type repoSubnavData struct {
	Issues int64
	Pulls  int64
	Forks  int64
}

// subnavCounts returns issue / PR / fork counts for the repo's
// header subnav. Counts are visibility-aware via the underlying
// queries (issues + pulls live in the same row table; the count
// query honors the issue_kind discriminator so closed counts and
// PR counts don't bleed into the issue badge).
func (h *Handlers) subnavCounts(ctx context.Context, repoID, forkCount int64) repoSubnavData {
	out := repoSubnavData{Forks: forkCount}
	openText := pgtype.Text{String: "open", Valid: true}
	if n, err := h.iq.CountIssues(ctx, h.d.Pool, issuesdb.CountIssuesParams{
		RepoID:      repoID,
		StateFilter: openText,
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindIssue, Valid: true},
	}); err == nil {
		out.Issues = n
	}
	if n, err := h.iq.CountIssues(ctx, h.d.Pool, issuesdb.CountIssuesParams{
		RepoID:      repoID,
		StateFilter: openText,
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindPr, Valid: true},
	}); err == nil {
		out.Pulls = n
	}
	return out
}

// canViewSettings reports whether the viewer should see the Settings
// tab. We surface it for any logged-in user who could plausibly have
// admin (the actual gate is server-side); for anonymous viewers we
// hide it.
func (h *Handlers) canViewSettings(viewer middleware.CurrentUser) bool {
	return !viewer.IsAnonymous()
}
