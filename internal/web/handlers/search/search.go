// SPDX-License-Identifier: AGPL-3.0-or-later

// Package search wires the S28 web search surface. The full results
// page lives at GET /search; the nav quick dropdown lives at GET
// /search/quick.
package search

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	srch "github.com/tenseleyFlow/shithub/internal/search"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps wires the handler set.
type Deps struct {
	Logger *slog.Logger
	Render *render.Renderer
	Pool   *pgxpool.Pool
}

// Handlers is the registered handler set. Construct via New.
type Handlers struct {
	d Deps
}

// New constructs the handler set, validating Deps.
func New(d Deps) (*Handlers, error) {
	if d.Render == nil {
		return nil, errors.New("search: nil Render")
	}
	if d.Pool == nil {
		return nil, errors.New("search: nil Pool")
	}
	return &Handlers{d: d}, nil
}

// Mount registers /search and /search/quick.
func (h *Handlers) Mount(r chi.Router) {
	r.Get("/search", h.results)
	r.Get("/search/quick", h.quick)
}

func (h *Handlers) deps() srch.Deps {
	return srch.Deps{Pool: h.d.Pool, Logger: h.d.Logger}
}

func (h *Handlers) actor(r *http.Request) policy.Actor {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		return policy.AnonymousActor()
	}
	return policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
}

// results renders the full /search page with type tabs.
func (h *Handlers) results(w http.ResponseWriter, r *http.Request) {
	rawQ := r.URL.Query().Get("q")
	tab := normalizeSearchTab(r.URL.Query().Get("type"))
	page := pageFromRequest(r)

	parsed := srch.ParseQuery(rawQ)
	actor := h.actor(r)
	deps := h.deps()

	data := map[string]any{
		"Title":             "Search",
		"Query":             rawQ,
		"GlobalSearchQuery": rawQ,
		"Tab":               tab,
		"Page":              page,
		"Parsed":            parsed,
		"PageSize":          srch.PageSize,
		"SearchProTip":      searchProTip(tab),
	}

	if !parsed.HasContent() {
		data["EmptyQuery"] = true
		data["SearchTabs"] = h.searchTabs(r, actor, parsed, rawQ, tab)
		_ = h.d.Render.RenderPage(w, r, "search/results", data)
		return
	}

	offset := (page - 1) * srch.PageSize
	switch tab {
	case "repositories":
		rows, total, err := srch.SearchRepos(r.Context(), deps, actor, parsed, srch.PageSize, offset)
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search repos", "error", err)
		}
		data["Repos"] = rows
		data["Total"] = total
		data["HasNext"] = int64(page*srch.PageSize) < total
	case "issues":
		rows, total, err := srch.SearchIssues(r.Context(), deps, actor, parsed, "issue", srch.PageSize, offset)
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search issues", "error", err)
		}
		data["Issues"] = rows
		data["Total"] = total
		data["HasNext"] = int64(page*srch.PageSize) < total
	case "pullrequests":
		rows, total, err := srch.SearchIssues(r.Context(), deps, actor, parsed, "pr", srch.PageSize, offset)
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search pulls", "error", err)
		}
		data["Issues"] = rows
		data["Total"] = total
		data["HasNext"] = int64(page*srch.PageSize) < total
	case "users":
		rows, total, err := srch.SearchUsers(r.Context(), deps, parsed, srch.PageSize, offset)
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search users", "error", err)
		}
		data["Users"] = rows
		data["Total"] = total
		data["HasNext"] = int64(page*srch.PageSize) < total
	case "code":
		rows, total, err := srch.SearchCode(r.Context(), deps, actor, parsed, srch.PageSize, offset)
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search code", "error", err)
		}
		data["Code"] = rows
		data["Total"] = total
		data["HasNext"] = int64(page*srch.PageSize) < total
	default:
		// Unknown tab → render the page with the empty-state shape
		// rather than 400 (a typo in the URL shouldn't be a hard
		// error).
		data["EmptyQuery"] = true
	}
	data["HasPrev"] = page > 1
	data["SearchTabs"] = h.searchTabs(r, actor, parsed, rawQ, tab)
	data["ResultHeading"] = searchResultHeading(tab, data["Total"])
	if page > 1 {
		data["PrevHref"] = searchHref(rawQ, tab, page-1)
	}
	if next, ok := data["HasNext"].(bool); ok && next {
		data["NextHref"] = searchHref(rawQ, tab, page+1)
	}

	if err := h.d.Render.RenderPage(w, r, "search/results", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "search render", "error", err)
	}
}

func normalizeSearchTab(tab string) string {
	switch tab {
	case "", "repos", "repositories":
		return "repositories"
	case "code":
		return "code"
	case "issues":
		return "issues"
	case "pulls", "pullrequests":
		return "pullrequests"
	case "users":
		return "users"
	default:
		return "repositories"
	}
}

type searchTab struct {
	Key      string
	Label    string
	Icon     string
	Count    int64
	Href     string
	Selected bool
}

func (h *Handlers) searchTabs(r *http.Request, actor policy.Actor, parsed srch.ParsedQuery, rawQ, active string) []searchTab {
	tabs := []searchTab{
		{Key: "code", Label: "Code", Icon: "code"},
		{Key: "repositories", Label: "Repositories", Icon: "repo"},
		{Key: "issues", Label: "Issues", Icon: "issue-opened"},
		{Key: "pullrequests", Label: "Pull requests", Icon: "git-pull-request"},
		{Key: "users", Label: "Users", Icon: "people"},
	}
	for i := range tabs {
		tabs[i].Selected = tabs[i].Key == active
		tabs[i].Href = searchHref(rawQ, tabs[i].Key, 1)
	}
	if !parsed.HasContent() {
		return tabs
	}

	deps := h.deps()
	for i := range tabs {
		var total int64
		var err error
		switch tabs[i].Key {
		case "repositories":
			_, total, err = srch.SearchRepos(r.Context(), deps, actor, parsed, 0, 0)
		case "code":
			_, total, err = srch.SearchCode(r.Context(), deps, actor, parsed, 0, 0)
		case "issues":
			_, total, err = srch.SearchIssues(r.Context(), deps, actor, parsed, "issue", 0, 0)
		case "pullrequests":
			_, total, err = srch.SearchIssues(r.Context(), deps, actor, parsed, "pr", 0, 0)
		case "users":
			_, total, err = srch.SearchUsers(r.Context(), deps, parsed, 0, 0)
		}
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search tab count", "tab", tabs[i].Key, "error", err)
			continue
		}
		tabs[i].Count = total
	}
	return tabs
}

func searchHref(q, tab string, page int) string {
	v := url.Values{}
	v.Set("q", q)
	v.Set("type", tab)
	if page > 1 {
		v.Set("p", intString(page))
	}
	return "/search?" + v.Encode()
}

func searchResultHeading(tab string, total any) string {
	count, _ := total.(int64)
	switch tab {
	case "code":
		return plural(count, "code result", "code results")
	case "issues":
		return plural(count, "issue result", "issue results")
	case "pullrequests":
		return plural(count, "pull request result", "pull request results")
	case "users":
		return plural(count, "user result", "user results")
	default:
		return plural(count, "repository result", "repository results")
	}
}

func plural(count int64, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return int64String(count) + " " + many
}

func int64String(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func searchProTip(tab string) string {
	switch tab {
	case "issues", "pullrequests":
		return "Restrict your search to the title by using the in:title qualifier."
	case "code":
		return "Use repo:owner/name to limit code search to a single repository."
	case "users":
		return "Search by username or display name to find people faster."
	default:
		return "Press / to activate the search input again and adjust your query."
	}
}

// quick is the nav dropdown endpoint. Returns one fragment with
// the top N results across the implemented quick-search types.
func (h *Handlers) quick(w http.ResponseWriter, r *http.Request) {
	rawQ := r.URL.Query().Get("q")
	parsed := srch.ParseQuery(rawQ)
	if !parsed.HasContent() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	actor := h.actor(r)
	deps := h.deps()

	repos, _, _ := srch.SearchRepos(r.Context(), deps, actor, parsed, srch.QuickResultsLimit, 0)
	issues, _, _ := srch.SearchIssues(r.Context(), deps, actor, parsed, "", srch.QuickResultsLimit, 0)
	users, _, _ := srch.SearchUsers(r.Context(), deps, parsed, srch.QuickResultsLimit, 0)

	data := map[string]any{
		"Query":      rawQ,
		"SearchHref": searchHref(rawQ, "repositories", 1),
		"Repos":      repos,
		"Issues":     issues,
		"Users":      users,
	}
	if err := h.d.Render.RenderFragment(w, "search/_quick_dropdown", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "quick render", "error", err)
	}
}

// pageFromRequest pulls ?page=N, defaulting to 1 on missing/invalid.
func pageFromRequest(r *http.Request) int {
	p := r.URL.Query().Get("p")
	if p == "" {
		p = r.URL.Query().Get("page")
	}
	if p == "" {
		return 1
	}
	n := 0
	for _, c := range p {
		if c < '0' || c > '9' {
			return 1
		}
		n = n*10 + int(c-'0')
		if n > 10000 {
			return 1
		}
	}
	if n < 1 {
		return 1
	}
	return n
}

func intString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
