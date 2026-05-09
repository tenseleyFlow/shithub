// SPDX-License-Identifier: AGPL-3.0-or-later

// Package search wires the S28 web search surface. The full results
// page lives at GET /search; the htmx quick dropdown lives at GET
// /search/quick.
package search

import (
	"errors"
	"log/slog"
	"net/http"

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
	tab := r.URL.Query().Get("type")
	if tab == "" {
		tab = "repos"
	}
	page := pageFromRequest(r)

	parsed := srch.ParseQuery(rawQ)
	actor := h.actor(r)
	deps := h.deps()

	data := map[string]any{
		"Title":    "Search",
		"Query":    rawQ,
		"Tab":      tab,
		"Page":     page,
		"Parsed":   parsed,
		"PageSize": srch.PageSize,
	}

	if !parsed.HasContent() {
		data["EmptyQuery"] = true
		_ = h.d.Render.RenderPage(w, r, "search/results", data)
		return
	}

	offset := (page - 1) * srch.PageSize
	switch tab {
	case "repos":
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

	if err := h.d.Render.RenderPage(w, r, "search/results", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "search render", "error", err)
	}
}

// quick is the htmx dropdown endpoint. Returns one fragment with
// the top N results across all four types stacked vertically.
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
		"Query":  rawQ,
		"Repos":  repos,
		"Issues": issues,
		"Users":  users,
	}
	if err := h.d.Render.RenderPage(w, r, "search/_quick_dropdown", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "quick render", "error", err)
	}
}

// pageFromRequest pulls ?page=N, defaulting to 1 on missing/invalid.
func pageFromRequest(r *http.Request) int {
	p := r.URL.Query().Get("page")
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
