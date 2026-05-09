// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const starsTabPageSize = 30

// serveStarsTab renders the `/{user}?tab=stars` view.
//
// The query returns every star (including private-repo stars); we
// post-filter per-row by `policy.IsVisibleTo` so the viewer only
// sees stars on repos they can read. Anonymous viewers see only
// public stars; the user themselves sees everything.
func (h *Handlers) serveStarsTab(w http.ResponseWriter, r *http.Request, user usersdb.User, viewer middleware.CurrentUser, isSelf bool) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	// We over-fetch a little so the per-row visibility filter can
	// drop private-repo rows without leaving a short page. With most
	// stars being public this is a no-op; for users with many private
	// stars the filter just trims more.
	const fetch = starsTabPageSize * 2
	rows, err := socialdb.New().ListStarsForUser(r.Context(), h.d.Pool, socialdb.ListStarsForUserParams{
		UserID: user.ID,
		Limit:  fetch,
		Offset: int32((page - 1) * starsTabPageSize),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "stars tab: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	actor := policy.AnonymousActor()
	if !viewer.IsAnonymous() {
		actor = policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	}
	deps := policy.Deps{Pool: h.d.Pool}

	visible := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if len(visible) >= starsTabPageSize {
			break
		}
		// Re-check visibility per row. Star is on a repo that may have
		// flipped visibility between star-time and view-time.
		ref := policy.RepoRef{
			ID:          row.RepoID,
			OwnerUserID: row.OwnerUserID.Int64,
			OwnerOrgID:  row.OwnerOrgID.Int64,
			Visibility:  string(row.Visibility),
		}
		if !policy.IsVisibleTo(r.Context(), deps, actor, ref) {
			continue
		}
		// Resolve the owner's username for the link. The S31 org case
		// adds an org-username path; for now user-owned only.
		ownerName := ""
		if row.OwnerUserID.Valid {
			if u, err := h.q.GetUserByID(r.Context(), h.d.Pool, row.OwnerUserID.Int64); err == nil {
				ownerName = u.Username
			}
		}
		visible = append(visible, map[string]any{
			"OwnerName":       ownerName,
			"RepoName":        row.RepoName,
			"Description":     row.Description,
			"Visibility":      string(row.Visibility),
			"StarCount":       row.StarCount,
			"PrimaryLanguage": pgTextStringOrEmpty(row.PrimaryLanguage),
			"StarredAt":       row.StarredAt.Time,
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
		"Title":     "Stars · " + user.DisplayName,
		"User":      user,
		"IsSelf":    isSelf,
		"AvatarURL": avatarURL,
		"Stars":     visible,
		"Page":      page,
		"HasNext":   len(visible) >= starsTabPageSize,
		"HasPrev":   page > 1,
		"Tabs":      tabs,
		"ActiveTab": "stars",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.d.Render.RenderPage(w, r, "profile/stars_tab", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "stars tab: render", "error", err)
	}
}

// pgTextStringOrEmpty unwraps a nullable text column to "" when null.
func pgTextStringOrEmpty(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}
