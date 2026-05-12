// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const (
	starsTabPageSize  = 30
	starsTabScanLimit = 1000
)

type profileStarFilters struct {
	Query    string
	Type     string
	Language string
	Sort     string
}

type profileStarItem struct {
	OwnerName       string
	RepoName        string
	FullName        string
	URL             string
	Description     string
	Visibility      string
	IsPrivate       bool
	StarCount       int64
	PrimaryLanguage string
	LanguageColor   template.CSS
	UpdatedAt       time.Time
	StarredAt       time.Time
}

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
	filters := profileStarFilters{
		Query:    strings.TrimSpace(r.URL.Query().Get("q")),
		Type:     normalizeStarType(r.URL.Query().Get("type")),
		Language: strings.TrimSpace(r.URL.Query().Get("language")),
		Sort:     normalizeStarSort(r.URL.Query().Get("sort")),
	}

	rows, err := socialdb.New().ListStarsForUser(r.Context(), h.d.Pool, socialdb.ListStarsForUserParams{
		UserID: user.ID,
		Limit:  starsTabScanLimit,
		Offset: 0,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "stars tab: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	actor := policy.AnonymousActor()
	if !viewer.IsAnonymous() {
		actor = viewer.PolicyActor()
	}
	deps := policy.Deps{Pool: h.d.Pool}

	visible := make([]profileStarItem, 0, len(rows))
	for _, row := range rows {
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
		ownerName := row.OwnerSlug
		if ownerName == "" {
			continue
		}
		language := pgTextStringOrEmpty(row.PrimaryLanguage)
		visible = append(visible, profileStarItem{
			OwnerName:       ownerName,
			RepoName:        row.RepoName,
			FullName:        ownerName + "/" + row.RepoName,
			URL:             "/" + url.PathEscape(ownerName) + "/" + url.PathEscape(row.RepoName),
			Description:     row.Description,
			Visibility:      string(row.Visibility),
			IsPrivate:       ref.IsPrivate(),
			StarCount:       row.StarCount,
			PrimaryLanguage: language,
			LanguageColor:   template.CSS(orgLanguageColor(language)), //nolint:gosec // server-side constant map.
			UpdatedAt:       row.UpdatedAt.Time,
			StarredAt:       row.StarredAt.Time,
		})
	}

	languageOptions := starLanguageOptions(visible)
	filtered := filterProfileStars(visible, filters)
	sortProfileStars(filtered, filters.Sort)
	totalFiltered := len(filtered)
	start := (page - 1) * starsTabPageSize
	if start > totalFiltered {
		start = totalFiltered
	}
	end := start + starsTabPageSize
	if end > totalFiltered {
		end = totalFiltered
	}
	paged := filtered[start:end]

	if isSelf {
		w.Header().Set("Cache-Control", "no-cache, private")
	} else {
		w.Header().Set("Cache-Control", "max-age=120")
	}

	avatarURL := fmt.Sprintf("/avatars/%s", url.PathEscape(user.Username))
	displayName := user.DisplayName
	if displayName == "" {
		displayName = user.Username
	}
	followState := h.userFollowState(r.Context(), user.ID, viewer)
	tabs := h.tabCounts(r.Context(), user.ID, viewer)
	data := map[string]any{
		"Title":            "Stars · " + displayName,
		"User":             user,
		"DisplayName":      displayName,
		"IsSelf":           isSelf,
		"AvatarURL":        avatarURL,
		"JoinedFormatted":  user.CreatedAt.Time.Format("January 2, 2006"),
		"WebsiteSafe":      safeWebsite(user.Website),
		"FollowersCount":   followState.FollowersCount,
		"FollowingCount":   followState.FollowingCount,
		"Orgs":             h.profileOrganizations(r.Context(), user.ID),
		"Stars":            paged,
		"StarTotal":        len(visible),
		"FilteredCount":    totalFiltered,
		"LanguageOptions":  languageOptions,
		"StarFilters":      filters,
		"Page":             page,
		"HasNext":          end < totalFiltered,
		"HasPrev":          page > 1,
		"NextHref":         starsPageHref(user.Username, r.URL.Query(), page+1),
		"PrevHref":         starsPageHref(user.Username, r.URL.Query(), page-1),
		"Tabs":             tabs,
		"ActiveTab":        "stars",
		"StarsSearchLabel": starsSearchLabel(filters),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.d.Render.RenderPage(w, r, "profile/stars_tab", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "stars tab: render", "error", err)
	}
}

func normalizeStarType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "public", "private":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return "all"
	}
}

func normalizeStarSort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "recently-active", "stars":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return "recently-starred"
	}
}

func starLanguageOptions(items []profileStarItem) []string {
	seen := map[string]struct{}{}
	for _, item := range items {
		if item.PrimaryLanguage != "" {
			seen[item.PrimaryLanguage] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for language := range seen {
		out = append(out, language)
	}
	sort.Strings(out)
	return out
}

func filterProfileStars(items []profileStarItem, filters profileStarFilters) []profileStarItem {
	query := strings.ToLower(filters.Query)
	out := make([]profileStarItem, 0, len(items))
	for _, item := range items {
		if filters.Type == "public" && item.IsPrivate {
			continue
		}
		if filters.Type == "private" && !item.IsPrivate {
			continue
		}
		if filters.Language != "" && item.PrimaryLanguage != filters.Language {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(item.FullName+" "+item.Description+" "+item.PrimaryLanguage), query) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func sortProfileStars(items []profileStarItem, sortMode string) {
	sort.SliceStable(items, func(i, j int) bool {
		switch sortMode {
		case "recently-active":
			if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
				return items[i].UpdatedAt.After(items[j].UpdatedAt)
			}
		case "stars":
			if items[i].StarCount != items[j].StarCount {
				return items[i].StarCount > items[j].StarCount
			}
		default:
			if !items[i].StarredAt.Equal(items[j].StarredAt) {
				return items[i].StarredAt.After(items[j].StarredAt)
			}
		}
		return items[i].FullName < items[j].FullName
	})
}

func starsPageHref(username string, values url.Values, page int) string {
	next := url.Values{}
	for key, vals := range values {
		if key == "page" {
			continue
		}
		for _, val := range vals {
			next.Add(key, val)
		}
	}
	next.Set("tab", "stars")
	if page > 1 {
		next.Set("page", strconv.Itoa(page))
	}
	return "/" + url.PathEscape(username) + "?" + next.Encode()
}

func starsSearchLabel(filters profileStarFilters) string {
	parts := make([]string, 0, 3)
	if filters.Query != "" {
		parts = append(parts, "matching "+filters.Query)
	}
	if filters.Type != "all" {
		parts = append(parts, filters.Type)
	}
	if filters.Language != "" {
		parts = append(parts, filters.Language)
	}
	if len(parts) == 0 {
		return "Starred repositories"
	}
	return "Starred repositories " + strings.Join(parts, ", ")
}

// pgTextStringOrEmpty unwraps a nullable text column to "" when null.
func pgTextStringOrEmpty(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}
