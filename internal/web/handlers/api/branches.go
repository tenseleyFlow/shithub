// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/protection"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountBranches registers the S50 §9 branches + tags REST surface.
//
//	GET /api/v1/repos/{o}/{r}/branches[?page=&per_page=]   list branches
//	GET /api/v1/repos/{o}/{r}/branches/{name}              single branch
//	GET /api/v1/repos/{o}/{r}/tags[?page=&per_page=]       list tags
//
// All endpoints require `repo:read` and gate on `ActionRepoRead`.
// Empty / uninitialised repos return an empty list (not 404).
func (h *Handlers) mountBranches(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/branches", h.branchesList)
		// Branches can contain `/` (`feature/x`, `release/v1.0`), so
		// we use a wildcard segment and pull the captured remainder
		// from chi via the `*` param. URL-encoded slashes also work.
		r.Get("/api/v1/repos/{owner}/{repo}/branches/*", h.branchGet)
		r.Get("/api/v1/repos/{owner}/{repo}/tags", h.tagsList)
	})
}

type branchResponse struct {
	Name      string `json:"name"`
	CommitSHA string `json:"commit_sha"`
	Protected bool   `json:"protected"`
	IsDefault bool   `json:"is_default"`
}

type tagResponse struct {
	Name      string `json:"name"`
	CommitSHA string `json:"commit_sha"`
}

func (h *Handlers) branchesList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gitDir, err := h.repoGitDir(r.Context(), repo)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: repoGitDir", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	refs, err := git.ListRefs(r.Context(), gitDir)
	if err != nil {
		// Treat a missing / uninitialised repo dir as empty rather
		// than 500. ListRefs returns wrapped exec errors; we don't
		// have a fine-grained sentinel so check by behaviour: just
		// hand the caller an empty list.
		h.d.Logger.WarnContext(r.Context(), "api: ListRefs", "error", err, "repo_id", repo.ID)
		writeJSON(w, http.StatusOK, []branchResponse{})
		return
	}
	rules := h.fetchProtectionRules(r, repo.ID)
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	total := len(refs.Branches)
	start, end := paginateBounds(page, perPage, total)
	slice := refs.Branches[start:end]
	out := make([]branchResponse, 0, len(slice))
	for _, b := range slice {
		_, isProtected := protection.MatchLongestRule(rules, b.Name)
		out = append(out, branchResponse{
			Name:      b.Name,
			CommitSHA: b.OID,
			Protected: isProtected,
			IsDefault: b.Name == repo.DefaultBranch,
		})
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: total}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) branchGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	name := chi.URLParam(r, "*")
	gitDir, err := h.repoGitDir(r.Context(), repo)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	refs, err := git.ListRefs(r.Context(), gitDir)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "branch not found")
		return
	}
	for _, b := range refs.Branches {
		if b.Name != name {
			continue
		}
		rules := h.fetchProtectionRules(r, repo.ID)
		_, isProtected := protection.MatchLongestRule(rules, b.Name)
		writeJSON(w, http.StatusOK, branchResponse{
			Name:      b.Name,
			CommitSHA: b.OID,
			Protected: isProtected,
			IsDefault: b.Name == repo.DefaultBranch,
		})
		return
	}
	writeAPIError(w, http.StatusNotFound, "branch not found")
}

func (h *Handlers) tagsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gitDir, err := h.repoGitDir(r.Context(), repo)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	refs, err := git.ListRefs(r.Context(), gitDir)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "api: ListRefs", "error", err, "repo_id", repo.ID)
		writeJSON(w, http.StatusOK, []tagResponse{})
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	total := len(refs.Tags)
	start, end := paginateBounds(page, perPage, total)
	slice := refs.Tags[start:end]
	out := make([]tagResponse, 0, len(slice))
	for _, t := range slice {
		out = append(out, tagResponse{Name: t.Name, CommitSHA: t.OID})
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: total}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

// fetchProtectionRules pulls the repo's branch-protection rule set.
// Errors fall back to "no rules" — the caller renders branches
// unmarked rather than 500ing on protection-row read failure.
func (h *Handlers) fetchProtectionRules(r *http.Request, repoID int64) []reposdb.BranchProtectionRule {
	rules, err := reposdb.New().ListBranchProtectionRules(r.Context(), h.d.Pool, repoID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "api: ListBranchProtectionRules", "error", err, "repo_id", repoID)
		return nil
	}
	return rules
}

// paginateBounds clamps [start, end) for a 1-indexed page.
func paginateBounds(page, perPage, total int) (start, end int) {
	if perPage <= 0 {
		perPage = apipage.DefaultPerPage
	}
	start = (page - 1) * perPage
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end = start + perPage
	if end > total {
		end = total
	}
	return start, end
}
