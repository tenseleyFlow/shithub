// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"context"
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
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const userRepositoriesPerPage = 30

type userRepositoryFilters struct {
	Query    string
	Type     string
	Language string
	Sort     string
}

type userRepositoryItem struct {
	ID                   int64
	Name                 string
	Description          string
	Visibility           string
	IsArchived           bool
	IsFork               bool
	Public               bool
	Private              bool
	Source               bool
	Archived             bool
	PrimaryLanguage      string
	PrimaryLanguageColor template.CSS
	LicenseKey           string
	StarCount            int64
	ForkCount            int64
	UpdatedAt            time.Time
	DefaultBranch        string
	ActivitySparkline    template.HTML
}

// serveRepositoriesTab renders /{user}?tab=repositories with the
// viewer-visible subset of the user's owned repos. Visibility scoping
// reuses policy.IsVisibleTo so anonymous viewers and non-collab
// logged-in viewers see only public repos; the user themselves sees
// everything (including private + archived).
func (h *Handlers) serveRepositoriesTab(w http.ResponseWriter, r *http.Request, user usersdb.User, viewer middleware.CurrentUser, isSelf bool) {
	ctx := r.Context()
	filters := userRepositoryFilters{
		Query:    strings.TrimSpace(r.URL.Query().Get("q")),
		Type:     normalizeUserRepositoryType(r.URL.Query().Get("type")),
		Language: strings.TrimSpace(r.URL.Query().Get("language")),
		Sort:     normalizeUserRepositorySort(r.URL.Query().Get("sort")),
	}
	page := parseUserRepositoryPage(r.URL.Query().Get("page"))

	all, err := reposdb.New().ListReposForOwnerUser(r.Context(), h.d.Pool,
		pgtype.Int8{Int64: user.ID, Valid: true})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repos tab: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	actor := policy.AnonymousActor()
	if !viewer.IsAnonymous() {
		actor = viewer.PolicyActor()
	}
	deps := policy.Deps{Pool: h.d.Pool}

	rows := make([]userRepositoryItem, 0, len(all))
	for _, repo := range all {
		ref := policy.NewRepoRefFromRepo(repo)
		if !policy.IsVisibleTo(r.Context(), deps, actor, ref) {
			continue
		}
		language := pgTextStringOrEmpty(repo.PrimaryLanguage)
		item := userRepositoryItem{
			ID:              repo.ID,
			Name:            string(repo.Name),
			Description:     repo.Description,
			Visibility:      string(repo.Visibility),
			IsArchived:      repo.IsArchived,
			IsFork:          repo.ForkOfRepoID.Valid,
			Public:          ref.IsPublic(),
			Private:         ref.IsPrivate(),
			Source:          !repo.ForkOfRepoID.Valid && !repo.IsArchived,
			Archived:        repo.IsArchived,
			PrimaryLanguage: language,
			LicenseKey:      pgTextStringOrEmpty(repo.LicenseKey),
			StarCount:       repo.StarCount,
			ForkCount:       repo.ForkCount,
			UpdatedAt:       repo.UpdatedAt.Time,
			DefaultBranch:   repo.DefaultBranch,
		}
		item.PrimaryLanguageColor = template.CSS(orgLanguageColor(language)) //nolint:gosec // CSS value comes from server-side constants.
		rows = append(rows, item)
	}

	filtered := filterUserRepositories(rows, filters)
	sortUserRepositories(filtered, filters.Sort)
	pageRepos, currentPage, pageCount := paginateUserRepositories(filtered, page)
	pageRepos = h.withUserRepoActivity(ctx, user.Username, pageRepos)

	if isSelf {
		w.Header().Set("Cache-Control", "no-cache, private")
	} else {
		w.Header().Set("Cache-Control", "max-age=120")
	}
	avatarURL := fmt.Sprintf("/avatars/%s", url.PathEscape(user.Username))
	tabs := h.tabCounts(r.Context(), user.ID, viewer)
	displayName := user.DisplayName
	if displayName == "" {
		displayName = user.Username
	}
	followState := h.userFollowState(r.Context(), user.ID, viewer)
	typeFilters := userRepositoryTypeFilters(user.Username, rows, filters)
	languageFilters := userRepositoryLanguageFilters(user.Username, rows, filters)
	sortOptions := userRepositorySortOptions(user.Username, filters)
	data := map[string]any{
		"Title":                 "Repositories · " + displayName,
		"User":                  user,
		"DisplayName":           displayName,
		"IsSelf":                isSelf,
		"AvatarURL":             avatarURL,
		"JoinedFormatted":       user.CreatedAt.Time.Format("January 2, 2006"),
		"WebsiteSafe":           safeWebsite(user.Website),
		"FollowersCount":        followState.FollowersCount,
		"FollowingCount":        followState.FollowingCount,
		"Orgs":                  h.profileOrganizations(r.Context(), user.ID),
		"Repos":                 pageRepos,
		"RepoTotal":             len(rows),
		"FilteredCount":         len(filtered),
		"HasActiveFilters":      filters.Query != "" || filters.Type != "all" || filters.Language != "" || filters.Sort != "updated",
		"RepositoryFilters":     filters,
		"SelectedType":          filters.Type,
		"SelectedTypeLabel":     selectedOrgRepositoryOptionLabel(typeFilters, "All"),
		"SelectedLanguage":      filters.Language,
		"SelectedLanguageLabel": selectedOrgRepositoryOptionLabel(languageFilters, "All languages"),
		"SelectedSort":          filters.Sort,
		"SelectedSortLabel":     selectedOrgRepositoryOptionLabel(sortOptions, "Last updated"),
		"TypeFilters":           typeFilters,
		"LanguageFilters":       languageFilters,
		"SortOptions":           sortOptions,
		"Page":                  currentPage,
		"PageCount":             pageCount,
		"PaginationPages":       userRepositoryPaginationPages(user.Username, filters, currentPage, pageCount),
		"HasPrev":               currentPage > 1 && pageCount > 0,
		"HasNext":               currentPage < pageCount,
		"PrevHref":              userRepositoryURL(user.Username, filters.Query, filters.Type, filters.Language, filters.Sort, currentPage-1),
		"NextHref":              userRepositoryURL(user.Username, filters.Query, filters.Type, filters.Language, filters.Sort, currentPage+1),
		"CanCreateRepo":         isSelf,
		"NewRepoHref":           "/new?owner=" + url.QueryEscape(user.Username),
		"Tabs":                  tabs,
		"ActiveTab":             "repositories",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.d.Render.RenderPage(w, r, "profile/repositories_tab", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repos tab: render", "error", err)
	}
}

func filterUserRepositories(repos []userRepositoryItem, filters userRepositoryFilters) []userRepositoryItem {
	query := strings.ToLower(strings.TrimSpace(filters.Query))
	languageFilter := strings.TrimSpace(filters.Language)
	out := make([]userRepositoryItem, 0, len(repos))
	for _, repo := range repos {
		if !userRepositoryMatchesType(repo, filters.Type) {
			continue
		}
		if languageFilter != "" && !strings.EqualFold(repo.PrimaryLanguage, languageFilter) {
			continue
		}
		if query != "" && !userRepositoryMatchesQuery(repo, query) {
			continue
		}
		out = append(out, repo)
	}
	return out
}

func userRepositoryMatchesType(repo userRepositoryItem, typeFilter string) bool {
	switch typeFilter {
	case "public":
		return repo.Public
	case "private":
		return repo.Private
	case "source":
		return repo.Source
	case "fork":
		return repo.IsFork
	case "archived":
		return repo.Archived
	default:
		return true
	}
}

func userRepositoryMatchesQuery(repo userRepositoryItem, query string) bool {
	return strings.Contains(strings.ToLower(repo.Name), query) ||
		strings.Contains(strings.ToLower(repo.Description), query) ||
		strings.Contains(strings.ToLower(repo.PrimaryLanguage), query) ||
		strings.Contains(strings.ToLower(repo.LicenseKey), query)
}

func sortUserRepositories(repos []userRepositoryItem, sortKey string) {
	sort.SliceStable(repos, func(i, j int) bool {
		switch sortKey {
		case "name":
			return strings.ToLower(repos[i].Name) < strings.ToLower(repos[j].Name)
		case "stars":
			if repos[i].StarCount != repos[j].StarCount {
				return repos[i].StarCount > repos[j].StarCount
			}
		}
		if !repos[i].UpdatedAt.Equal(repos[j].UpdatedAt) {
			return repos[i].UpdatedAt.After(repos[j].UpdatedAt)
		}
		return strings.ToLower(repos[i].Name) < strings.ToLower(repos[j].Name)
	})
}

func normalizeUserRepositoryType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "public", "private", "fork", "archived":
		return strings.ToLower(strings.TrimSpace(value))
	case "source", "sources":
		return "source"
	case "forks":
		return "fork"
	default:
		return "all"
	}
}

func normalizeUserRepositorySort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "name", "stars":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "updated"
	}
}

func parseUserRepositoryPage(value string) int {
	page, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

func paginateUserRepositories(repos []userRepositoryItem, page int) ([]userRepositoryItem, int, int) {
	if len(repos) == 0 {
		return nil, 1, 0
	}
	pageCount := (len(repos) + userRepositoriesPerPage - 1) / userRepositoriesPerPage
	if page > pageCount {
		page = pageCount
	}
	start := (page - 1) * userRepositoriesPerPage
	end := start + userRepositoriesPerPage
	if end > len(repos) {
		end = len(repos)
	}
	return repos[start:end], page, pageCount
}

func (h *Handlers) withUserRepoActivity(ctx context.Context, ownerSlug string, repos []userRepositoryItem) []userRepositoryItem {
	out := append([]userRepositoryItem(nil), repos...)
	for i := range out {
		out[i].ActivitySparkline = h.userRepoActivitySparkline(ctx, ownerSlug, out[i])
	}
	return out
}

func (h *Handlers) userRepoActivitySparkline(ctx context.Context, ownerSlug string, repo userRepositoryItem) template.HTML {
	if h.d.RepoFS == nil {
		return orgActivitySparklineSVG(nil)
	}
	gitDir, err := h.d.RepoFS.RepoPath(ownerSlug, repo.Name)
	if err != nil {
		return orgActivitySparklineSVG(nil)
	}
	buckets, err := repogit.WeeklyCommitActivity(ctx, gitDir, repo.DefaultBranch, 52, time.Now())
	if err != nil {
		return orgActivitySparklineSVG(nil)
	}
	return orgActivitySparklineSVG(buckets)
}

func userRepositoryURL(username, query, typeFilter, language, sortKey string, page int) string {
	values := url.Values{}
	values.Set("tab", "repositories")
	if query != "" {
		values.Set("q", query)
	}
	if typeFilter != "" && typeFilter != "all" {
		values.Set("type", typeFilter)
	}
	if language != "" {
		values.Set("language", language)
	}
	if sortKey != "" && sortKey != "updated" {
		values.Set("sort", sortKey)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	return "/" + url.PathEscape(username) + "?" + values.Encode()
}

func userRepositoryTypeFilters(username string, repos []userRepositoryItem, filters userRepositoryFilters) []orgRepositoryFilterOption {
	counts := map[string]int{"all": len(repos)}
	for _, repo := range repos {
		if repo.Public {
			counts["public"]++
		}
		if repo.Private {
			counts["private"]++
		}
		if repo.Source {
			counts["source"]++
		}
		if repo.IsFork {
			counts["fork"]++
		}
		if repo.Archived {
			counts["archived"]++
		}
	}
	specs := []struct {
		value string
		label string
	}{
		{value: "all", label: "All"},
		{value: "public", label: "Public"},
		{value: "private", label: "Private"},
		{value: "source", label: "Sources"},
		{value: "fork", label: "Forks"},
		{value: "archived", label: "Archived"},
	}
	out := make([]orgRepositoryFilterOption, 0, len(specs))
	for _, spec := range specs {
		if spec.value == "private" && counts[spec.value] == 0 && filters.Type != spec.value {
			continue
		}
		out = append(out, orgRepositoryFilterOption{
			Label:    spec.label,
			Value:    spec.value,
			Href:     userRepositoryURL(username, filters.Query, spec.value, filters.Language, filters.Sort, 1),
			Selected: filters.Type == spec.value,
			Count:    counts[spec.value],
		})
	}
	return out
}

func userRepositoryLanguageFilters(username string, repos []userRepositoryItem, filters userRepositoryFilters) []orgRepositoryFilterOption {
	counts := map[string]int{}
	for _, repo := range repos {
		if repo.PrimaryLanguage != "" {
			counts[repo.PrimaryLanguage]++
		}
	}
	languages := make([]string, 0, len(counts))
	for language := range counts {
		languages = append(languages, language)
	}
	sort.SliceStable(languages, func(i, j int) bool {
		if counts[languages[i]] != counts[languages[j]] {
			return counts[languages[i]] > counts[languages[j]]
		}
		return languages[i] < languages[j]
	})
	out := []orgRepositoryFilterOption{{
		Label:    "All languages",
		Value:    "",
		Href:     userRepositoryURL(username, filters.Query, filters.Type, "", filters.Sort, 1),
		Selected: filters.Language == "",
		Count:    len(repos),
	}}
	for _, language := range languages {
		out = append(out, orgRepositoryFilterOption{
			Label:    language,
			Value:    language,
			Href:     userRepositoryURL(username, filters.Query, filters.Type, language, filters.Sort, 1),
			Selected: strings.EqualFold(filters.Language, language),
			Count:    counts[language],
		})
	}
	return out
}

func userRepositorySortOptions(username string, filters userRepositoryFilters) []orgRepositoryFilterOption {
	specs := []struct {
		value string
		label string
	}{
		{value: "updated", label: "Last updated"},
		{value: "name", label: "Name"},
		{value: "stars", label: "Stars"},
	}
	out := make([]orgRepositoryFilterOption, 0, len(specs))
	for _, spec := range specs {
		out = append(out, orgRepositoryFilterOption{
			Label:    spec.label,
			Value:    spec.value,
			Href:     userRepositoryURL(username, filters.Query, filters.Type, filters.Language, spec.value, 1),
			Selected: filters.Sort == spec.value,
		})
	}
	return out
}

func userRepositoryPaginationPages(username string, filters userRepositoryFilters, page, pageCount int) []orgRepositoryPageLink {
	if pageCount <= 1 {
		return nil
	}
	out := make([]orgRepositoryPageLink, 0, pageCount)
	for i := 1; i <= pageCount; i++ {
		out = append(out, orgRepositoryPageLink{
			Number:  i,
			Href:    userRepositoryURL(username, filters.Query, filters.Type, filters.Language, filters.Sort, i),
			Current: i == page,
		})
	}
	return out
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
