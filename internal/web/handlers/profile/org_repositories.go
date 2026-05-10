// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const orgRepositoriesPerPage = 30

type orgRepositoryFilterOption struct {
	Label    string
	Value    string
	Href     string
	Selected bool
	Count    int
}

type orgRepositoryPageLink struct {
	Number  int
	Href    string
	Current bool
}

// MountOrgRepositories registers the GitHub-style organization
// repositories route. It deliberately uses /orgs/{org}/repositories
// instead of /{org}/repositories so ordinary repos named "repositories"
// remain reachable at /{owner}/repositories.
func (h *Handlers) MountOrgRepositories(r chi.Router) {
	r.Get("/orgs/{org}/repositories", h.serveOrgRepositories)
}

func (h *Handlers) serveOrgRepositories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org, err := orgsdb.New().GetOrgBySlug(ctx, h.d.Pool, chi.URLParam(r, "org"))
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}
	if org.DeletedAt.Valid {
		h.renderUnavailable(w, r, string(org.Slug))
		return
	}

	viewer := middleware.CurrentUserFromContext(ctx)
	isOwner, isMember, viewAs := h.orgViewerState(ctx, org.ID, viewer)
	repos := h.orgProfileRepos(ctx, org.ID, viewer)

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	typeFilter := normalizeOrgRepositoryType(r.URL.Query().Get("type"))
	languageFilter := strings.TrimSpace(r.URL.Query().Get("language"))
	sortKey := normalizeOrgRepositorySort(r.URL.Query().Get("sort"))

	filtered := filterOrgRepositories(repos, query, typeFilter, languageFilter)
	sortOrgRepositories(filtered, sortKey)
	page := parseOrgRepositoryPage(r.URL.Query().Get("page"))
	pageRepos, currentPage, pageCount := paginateOrgRepositories(filtered, page)
	pageRepos = h.withOrgRepoActivity(ctx, string(org.Slug), pageRepos)

	people := h.orgProfilePeople(ctx, orgsdb.New(), org.ID)
	teamCount := int64(0)
	if isMember || viewer.IsSiteAdmin {
		_ = h.d.Pool.QueryRow(ctx, `SELECT count(*) FROM teams WHERE org_id = $1`, org.ID).Scan(&teamCount)
	}

	baseURL := orgRepositoriesBaseURL(string(org.Slug))
	typeFilters := orgRepositoryTypeFilters(baseURL, repos, query, typeFilter, languageFilter, sortKey)
	languageFilters := orgRepositoryLanguageFilters(baseURL, repos, query, typeFilter, languageFilter, sortKey)
	sortOptions := orgRepositorySortOptions(baseURL, query, typeFilter, languageFilter, sortKey)
	avatarURL := "/avatars/" + url.PathEscape(string(org.Slug))
	titleName := org.DisplayName
	if titleName == "" {
		titleName = string(org.Slug)
	}
	data := map[string]any{
		"Title":                 titleName + " · repositories",
		"OGTitle":               titleName + " repositories",
		"OGDescription":         org.Description,
		"OGImage":               avatarURL,
		"Org":                   org,
		"AvatarURL":             avatarURL,
		"ActiveOrgNav":          "repositories",
		"Repos":                 pageRepos,
		"RepoCount":             int64(len(repos)),
		"FilteredCount":         len(filtered),
		"HasActiveFilters":      query != "" || typeFilter != "all" || languageFilter != "" || sortKey != "updated",
		"Query":                 query,
		"SelectedType":          typeFilter,
		"SelectedTypeLabel":     selectedOrgRepositoryOptionLabel(typeFilters, "All"),
		"SelectedLanguage":      languageFilter,
		"SelectedLanguageLabel": selectedOrgRepositoryOptionLabel(languageFilters, "All languages"),
		"SelectedSort":          sortKey,
		"SelectedSortLabel":     selectedOrgRepositoryOptionLabel(sortOptions, "Last updated"),
		"TypeFilters":           typeFilters,
		"LanguageFilters":       languageFilters,
		"SortOptions":           sortOptions,
		"Page":                  currentPage,
		"PageCount":             pageCount,
		"PaginationPages":       orgRepositoryPaginationPages(baseURL, query, typeFilter, languageFilter, sortKey, currentPage, pageCount),
		"HasPrev":               currentPage > 1 && pageCount > 0,
		"HasNext":               currentPage < pageCount,
		"PrevHref":              orgRepositoryURL(baseURL, query, typeFilter, languageFilter, sortKey, currentPage-1),
		"NextHref":              orgRepositoryURL(baseURL, query, typeFilter, languageFilter, sortKey, currentPage+1),
		"MemberCount":           int64(len(people)),
		"TeamCount":             teamCount,
		"ViewAs":                viewAs,
		"IsOwner":               isOwner,
		"IsMember":              isMember,
		"CanCreateRepo":         isOwner || (isMember && org.AllowMemberRepoCreate),
	}
	if !viewer.IsAnonymous() {
		w.Header().Set("Cache-Control", "no-cache, private")
	} else {
		w.Header().Set("Cache-Control", "max-age=120")
	}
	if err := h.d.Render.RenderPage(w, r, "orgs/repositories", data); err != nil {
		h.d.Logger.ErrorContext(ctx, "orgs repositories: render", "error", err)
	}
}

func (h *Handlers) orgViewerState(ctx context.Context, orgID int64, viewer middleware.CurrentUser) (bool, bool, string) {
	isOwner := false
	isMember := false
	if !viewer.IsAnonymous() {
		deps := orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}
		isOwner, _ = orgs.IsOwner(ctx, deps, orgID, viewer.ID)
		isMember, _ = orgs.IsMember(ctx, deps, orgID, viewer.ID)
	}
	viewAs := "Public"
	switch {
	case !viewer.IsAnonymous() && viewer.IsSiteAdmin:
		viewAs = "Site admin"
	case isOwner:
		viewAs = "Owner"
	case isMember:
		viewAs = "Member"
	}
	return isOwner, isMember, viewAs
}

func filterOrgRepositories(repos []orgProfileRepo, query, typeFilter, languageFilter string) []orgProfileRepo {
	query = strings.ToLower(strings.TrimSpace(query))
	languageFilter = strings.TrimSpace(languageFilter)
	out := make([]orgProfileRepo, 0, len(repos))
	for _, repo := range repos {
		if !orgRepositoryMatchesType(repo, typeFilter) {
			continue
		}
		if languageFilter != "" && !strings.EqualFold(repo.PrimaryLanguage, languageFilter) {
			continue
		}
		if query != "" && !orgRepositoryMatchesQuery(repo, query) {
			continue
		}
		out = append(out, repo)
	}
	return out
}

func orgRepositoryMatchesType(repo orgProfileRepo, typeFilter string) bool {
	switch typeFilter {
	case "public":
		return repo.Visibility == "public"
	case "private":
		return repo.Visibility == "private"
	case "source":
		return !repo.IsFork && !repo.IsArchived
	case "fork":
		return repo.IsFork
	case "archived":
		return repo.IsArchived
	default:
		return true
	}
}

func orgRepositoryMatchesQuery(repo orgProfileRepo, query string) bool {
	if strings.Contains(strings.ToLower(repo.Name), query) ||
		strings.Contains(strings.ToLower(repo.Description), query) ||
		strings.Contains(strings.ToLower(repo.PrimaryLanguage), query) {
		return true
	}
	for _, topic := range repo.Topics {
		if strings.Contains(strings.ToLower(topic), query) {
			return true
		}
	}
	return false
}

func sortOrgRepositories(repos []orgProfileRepo, sortKey string) {
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

func normalizeOrgRepositoryType(value string) string {
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

func normalizeOrgRepositorySort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "name", "stars":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "updated"
	}
}

func parseOrgRepositoryPage(value string) int {
	page, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

func paginateOrgRepositories(repos []orgProfileRepo, page int) ([]orgProfileRepo, int, int) {
	if len(repos) == 0 {
		return nil, 1, 0
	}
	pageCount := (len(repos) + orgRepositoriesPerPage - 1) / orgRepositoriesPerPage
	if page > pageCount {
		page = pageCount
	}
	start := (page - 1) * orgRepositoriesPerPage
	end := start + orgRepositoriesPerPage
	if end > len(repos) {
		end = len(repos)
	}
	return repos[start:end], page, pageCount
}

func orgRepositoriesBaseURL(orgSlug string) string {
	return "/orgs/" + url.PathEscape(orgSlug) + "/repositories"
}

func orgRepositoryURL(baseURL, query, typeFilter, languageFilter, sortKey string, page int) string {
	values := url.Values{}
	if query != "" {
		values.Set("q", query)
	}
	if typeFilter != "" && typeFilter != "all" {
		values.Set("type", typeFilter)
	}
	if languageFilter != "" {
		values.Set("language", languageFilter)
	}
	if sortKey != "" && sortKey != "updated" {
		values.Set("sort", sortKey)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if encoded := values.Encode(); encoded != "" {
		return baseURL + "?" + encoded
	}
	return baseURL
}

func orgRepositoryTypeFilters(baseURL string, repos []orgProfileRepo, query, selected, language, sortKey string) []orgRepositoryFilterOption {
	counts := map[string]int{"all": len(repos)}
	for _, repo := range repos {
		if repo.Visibility == "public" {
			counts["public"]++
		}
		if repo.Visibility == "private" {
			counts["private"]++
		}
		if !repo.IsFork && !repo.IsArchived {
			counts["source"]++
		}
		if repo.IsFork {
			counts["fork"]++
		}
		if repo.IsArchived {
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
		if spec.value == "private" && counts[spec.value] == 0 && selected != spec.value {
			continue
		}
		out = append(out, orgRepositoryFilterOption{
			Label:    spec.label,
			Value:    spec.value,
			Href:     orgRepositoryURL(baseURL, query, spec.value, language, sortKey, 1),
			Selected: selected == spec.value,
			Count:    counts[spec.value],
		})
	}
	return out
}

func orgRepositoryLanguageFilters(baseURL string, repos []orgProfileRepo, query, typeFilter, selected, sortKey string) []orgRepositoryFilterOption {
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
		Href:     orgRepositoryURL(baseURL, query, typeFilter, "", sortKey, 1),
		Selected: selected == "",
		Count:    len(repos),
	}}
	for _, language := range languages {
		out = append(out, orgRepositoryFilterOption{
			Label:    language,
			Value:    language,
			Href:     orgRepositoryURL(baseURL, query, typeFilter, language, sortKey, 1),
			Selected: strings.EqualFold(selected, language),
			Count:    counts[language],
		})
	}
	return out
}

func orgRepositorySortOptions(baseURL, query, typeFilter, language, selected string) []orgRepositoryFilterOption {
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
			Href:     orgRepositoryURL(baseURL, query, typeFilter, language, spec.value, 1),
			Selected: selected == spec.value,
		})
	}
	return out
}

func selectedOrgRepositoryOptionLabel(options []orgRepositoryFilterOption, fallback string) string {
	for _, option := range options {
		if option.Selected {
			return option.Label
		}
	}
	return fallback
}

func orgRepositoryPaginationPages(baseURL, query, typeFilter, language, sortKey string, page, pageCount int) []orgRepositoryPageLink {
	if pageCount <= 1 {
		return nil
	}
	out := make([]orgRepositoryPageLink, 0, pageCount)
	for i := 1; i <= pageCount; i++ {
		out = append(out, orgRepositoryPageLink{
			Number:  i,
			Href:    orgRepositoryURL(baseURL, query, typeFilter, language, sortKey, i),
			Current: i == page,
		})
	}
	return out
}
