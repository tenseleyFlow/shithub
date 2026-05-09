// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// serveRepositoriesTab renders /{user}?tab=repositories with the
// viewer-visible subset of the user's owned repos. Visibility scoping
// reuses policy.IsVisibleTo so anonymous viewers and non-collab
// logged-in viewers see only public repos; the user themselves sees
// everything (including private + archived).
func (h *Handlers) serveRepositoriesTab(w http.ResponseWriter, r *http.Request, user usersdb.User, viewer middleware.CurrentUser, isSelf bool) {
	all, err := reposdb.New().ListReposForOwnerUser(r.Context(), h.d.Pool,
		pgtype.Int8{Int64: user.ID, Valid: true})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repos tab: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	actor := policy.AnonymousActor()
	if !viewer.IsAnonymous() {
		actor = policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	}
	deps := policy.Deps{Pool: h.d.Pool}

	type item struct {
		Name            string
		Description     string
		Visibility      string
		IsArchived      bool
		IsFork          bool
		PrimaryLanguage string
		StarCount       int64
		ForkCount       int64
		UpdatedAt       string
	}
	rows := make([]item, 0, len(all))
	for _, repo := range all {
		ref := policy.NewRepoRefFromRepo(repo)
		if !policy.IsVisibleTo(r.Context(), deps, actor, ref) {
			continue
		}
		rows = append(rows, item{
			Name:            string(repo.Name),
			Description:     repo.Description,
			Visibility:      string(repo.Visibility),
			IsArchived:      repo.IsArchived,
			IsFork:          repo.ForkOfRepoID.Valid,
			PrimaryLanguage: pgTextStringOrEmpty(repo.PrimaryLanguage),
			StarCount:       repo.StarCount,
			ForkCount:       repo.ForkCount,
			UpdatedAt:       repo.UpdatedAt.Time.Format("Jan 2, 2006"),
		})
	}

	if isSelf {
		w.Header().Set("Cache-Control", "no-cache, private")
	} else {
		w.Header().Set("Cache-Control", "max-age=120")
	}
	avatarURL := fmt.Sprintf("/avatars/%s", url.PathEscape(user.Username))
	tabs := h.tabCounts(r.Context(), user.ID, viewer)
	data := map[string]any{
		"Title":     "Repositories · " + user.DisplayName,
		"User":      user,
		"IsSelf":    isSelf,
		"AvatarURL": avatarURL,
		"Repos":     rows,
		"Tabs":      tabs,
		"ActiveTab": "repositories",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.d.Render.RenderPage(w, r, "profile/repositories_tab", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repos tab: render", "error", err)
	}
}

// tabCounts returns the badge counts for the profile sub-nav. Counts
// are best-effort: a query failure collapses to 0 so the nav still
// renders. The repo count is visibility-aware (private repos hidden
// from non-self viewers) so the badge doesn't leak the existence of
// hidden repos.
func (h *Handlers) tabCounts(ctx context.Context, userID int64, viewer middleware.CurrentUser) map[string]int64 {
	out := map[string]int64{}
	isSelf := !viewer.IsAnonymous() && viewer.ID == userID

	visClause := " AND visibility = 'public'"
	if isSelf {
		visClause = ""
	}
	var repoCount, starCount int64
	_ = h.d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM repos
		  WHERE owner_user_id = $1 AND deleted_at IS NULL`+visClause,
		userID).Scan(&repoCount)
	_ = h.d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM stars WHERE user_id = $1`,
		userID).Scan(&starCount)
	out["repositories"] = repoCount
	out["stars"] = starCount
	return out
}
