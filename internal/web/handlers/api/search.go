// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/search"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountSearch registers the S50 §5 search REST surface. All endpoints
// accept anonymous callers — visibility is enforced inside the search
// package by passing the resolved policy.Actor (anonymous becomes the
// public-only filter).
//
//	GET /api/v1/search/repositories?q=...&page=N&per_page=M
//	GET /api/v1/search/issues?q=...&type=issue|pr
//	GET /api/v1/search/code?q=...
//
// Response shape mirrors GitHub:
//
//	{ "total_count": N, "incomplete_results": false, "items": [...] }
//
// `incomplete_results` is always false today — we never truncate FTS
// results below the requested page; the field is here for forward-
// compat with future ranker timeouts.
func (h *Handlers) mountSearch(r chi.Router) {
	r.Group(func(r chi.Router) {
		// Search is read-only against publicly-derivable indices, so the
		// scope gate is `repo:read`. Anonymous PATs (no token) skip the
		// scope check via PATAuthMiddleware's zero-value path and still
		// hit the policy visibility predicate inside the search package.
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/search/repositories", h.searchRepositories)
		r.Get("/api/v1/search/issues", h.searchIssues)
		r.Get("/api/v1/search/code", h.searchCode)
	})
}

// searchEnvelope is the canonical gh-compatible shape every endpoint
// returns. Stable across types; the `Items` slice carries whichever
// per-type response struct the caller hit.
type searchEnvelope struct {
	TotalCount        int64 `json:"total_count"`
	IncompleteResults bool  `json:"incomplete_results"`
	Items             any   `json:"items"`
}

// ─── repositories ───────────────────────────────────────────────────

type searchRepoItem struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	FullName    string  `json:"full_name"`
	OwnerLogin  string  `json:"owner_login"`
	Description string  `json:"description"`
	Visibility  string  `json:"visibility"`
	Private     bool    `json:"private"`
	StarCount   int64   `json:"star_count"`
	UpdatedAt   string  `json:"updated_at"`
	Score       float64 `json:"score"`
}

func (h *Handlers) searchRepositories(w http.ResponseWriter, r *http.Request) {
	q := search.ParseQuery(r.URL.Query().Get("q"))
	if !q.HasContent() {
		writeAPIError(w, http.StatusUnprocessableEntity, "query is empty")
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	rows, total, err := search.SearchRepos(r.Context(),
		search.Deps{Pool: h.d.Pool, Logger: h.d.Logger},
		auth.PolicyActor(), q, perPage, (page-1)*perPage)
	if err != nil {
		if errors.Is(err, search.ErrFTSStripped) {
			writeAPIError(w, http.StatusUnprocessableEntity, "query has no FTS-indexable tokens (try a longer or differently-worded query)")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: search repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "search failed")
		return
	}
	items := make([]searchRepoItem, 0, len(rows))
	for _, row := range rows {
		repoRef := policy.RepoRef{Visibility: row.Visibility}
		items = append(items, searchRepoItem{
			ID:          row.ID,
			Name:        row.Name,
			FullName:    row.OwnerUsername + "/" + row.Name,
			OwnerLogin:  row.OwnerUsername,
			Description: row.Description,
			Visibility:  row.Visibility,
			Private:     repoRef.IsPrivate(),
			StarCount:   row.StarCount,
			UpdatedAt:   row.UpdatedAt.UTC().Format(time.RFC3339),
			Score:       row.Rank,
		})
	}
	h.writeSearchEnvelope(w, r, page, perPage, total, items)
}

// ─── issues ─────────────────────────────────────────────────────────

type searchIssueItem struct {
	ID         int64   `json:"id"`
	Number     int64   `json:"number"`
	RepoID     int64   `json:"repo_id"`
	Repo       string  `json:"repo"`
	Title      string  `json:"title"`
	State      string  `json:"state"`
	Kind       string  `json:"kind"`
	AuthorName string  `json:"author_name"`
	UpdatedAt  string  `json:"updated_at"`
	Score      float64 `json:"score"`
	// G9a (F19): pre-fix `shithub status --json` rows had empty
	// `repository` and `url`. The shape mirrors the per-repo issue
	// response's `repository` envelope + `html_url`. Existing flat
	// `repo`/`repo_id` keys remain for one transition cycle.
	HTMLURL    string                   `json:"html_url,omitempty"`
	Repository *searchIssueRepoEnvelope `json:"repository,omitempty"`
}

// searchIssueRepoEnvelope is the trimmed repo node attached to each
// search-issue result so CLI clients can render `repository.full_name`
// and link to the repo without an extra fetch (F19).
type searchIssueRepoEnvelope struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url,omitempty"`
	Private  bool   `json:"private"`
}

func (h *Handlers) searchIssues(w http.ResponseWriter, r *http.Request) {
	q := search.ParseQuery(r.URL.Query().Get("q"))
	if !q.HasContent() {
		writeAPIError(w, http.StatusUnprocessableEntity, "query is empty")
		return
	}
	kindFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	switch kindFilter {
	case "issue", "pr", "":
		// ok
	default:
		writeAPIError(w, http.StatusUnprocessableEntity, "type must be issue or pr")
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	rows, total, err := search.SearchIssues(r.Context(),
		search.Deps{Pool: h.d.Pool, Logger: h.d.Logger},
		auth.PolicyActor(), q, kindFilter, perPage, (page-1)*perPage)
	if err != nil {
		if errors.Is(err, search.ErrFTSStripped) {
			writeAPIError(w, http.StatusUnprocessableEntity, "query has no FTS-indexable tokens (try a longer or differently-worded query)")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: search issues", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "search failed")
		return
	}
	items := make([]searchIssueItem, 0, len(rows))
	for _, row := range rows {
		fullName := row.OwnerUsername + "/" + row.RepoName
		// G9a (F19): build the URL with the correct issue/PR suffix —
		// /issues/{N} for kind='issue', /pulls/{N} for kind='pr'. Same
		// split that issueHTMLURL / pullHTMLURL maintain elsewhere.
		var htmlURL, repoURL string
		if h.d.BaseURL != "" {
			base := strings.TrimRight(h.d.BaseURL, "/") + "/" + fullName
			repoURL = base
			suffix := "/issues/"
			if row.Kind == "pr" {
				suffix = "/pulls/"
			}
			htmlURL = base + suffix + strconv.FormatInt(row.Number, 10)
		}
		items = append(items, searchIssueItem{
			ID:         row.ID,
			Number:     row.Number,
			RepoID:     row.RepoID,
			Repo:       fullName,
			Title:      row.Title,
			State:      row.State,
			Kind:       row.Kind,
			AuthorName: row.AuthorName,
			UpdatedAt:  row.UpdatedAt.UTC().Format(time.RFC3339),
			Score:      row.Rank,
			HTMLURL:    htmlURL,
			Repository: &searchIssueRepoEnvelope{
				ID:       row.RepoID,
				Name:     row.RepoName,
				FullName: fullName,
				HTMLURL:  repoURL,
				Private:  row.RepoVisibility != "public",
			},
		})
	}
	h.writeSearchEnvelope(w, r, page, perPage, total, items)
}

// ─── code ───────────────────────────────────────────────────────────

type searchCodeItem struct {
	RepoID      int64   `json:"repo_id"`
	Repo        string  `json:"repo"`
	Ref         string  `json:"ref"`
	Path        string  `json:"path"`
	PreviewLine string  `json:"preview_line,omitempty"`
	Score       float64 `json:"score"`
}

func (h *Handlers) searchCode(w http.ResponseWriter, r *http.Request) {
	q := search.ParseQuery(r.URL.Query().Get("q"))
	if !q.HasContent() {
		writeAPIError(w, http.StatusUnprocessableEntity, "query is empty")
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	rows, total, err := search.SearchCode(r.Context(),
		search.Deps{Pool: h.d.Pool, Logger: h.d.Logger},
		auth.PolicyActor(), q, perPage, (page-1)*perPage)
	if err != nil {
		if errors.Is(err, search.ErrFTSStripped) {
			writeAPIError(w, http.StatusUnprocessableEntity, "query has no FTS-indexable tokens (try a longer or differently-worded query)")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: search code", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "search failed")
		return
	}
	items := make([]searchCodeItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, searchCodeItem{
			RepoID:      row.RepoID,
			Repo:        row.OwnerUsername + "/" + row.RepoName,
			Ref:         row.RefName,
			Path:        row.Path,
			PreviewLine: row.PreviewLine,
			Score:       row.Rank,
		})
	}
	h.writeSearchEnvelope(w, r, page, perPage, total, items)
}

// writeSearchEnvelope writes the canonical `{ total_count,
// incomplete_results, items }` JSON wrapper and stamps the standard
// Link header for pagination.
func (h *Handlers) writeSearchEnvelope(w http.ResponseWriter, r *http.Request, page, perPage int, total int64, items any) {
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, searchEnvelope{
		TotalCount:        total,
		IncompleteResults: false,
		Items:             items,
	})
}
