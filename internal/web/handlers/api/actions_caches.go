// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountActionsCaches registers the S50 §13 caches REST surface.
//
//	GET    /api/v1/repos/{o}/{r}/actions/caches[?key=&ref=&page=&per_page=]
//	DELETE /api/v1/repos/{o}/{r}/actions/caches?key=...[&ref=...]
//	DELETE /api/v1/repos/{o}/{r}/actions/caches/{cache_id}
//
// Scopes: `repo:read` on the list, `repo:write` on the deletes.
//
// The runner-side upload protocol that populates this table is its
// own future sprint. This REST surface lands first so operators have
// observability and can purge stale entries by id or key, even
// before any cache rows exist.
func (h *Handlers) mountActionsCaches(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/actions/caches", h.actionsCachesList)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Delete("/api/v1/repos/{owner}/{repo}/actions/caches", h.actionsCachesDeleteByKey)
		r.Delete("/api/v1/repos/{owner}/{repo}/actions/caches/{cache_id}", h.actionsCacheDeleteByID)
	})
}

type cacheResponse struct {
	ID             int64  `json:"id"`
	Key            string `json:"key"`
	Version        string `json:"version"`
	Ref            string `json:"ref"`
	SizeBytes      int64  `json:"size_bytes"`
	LastAccessedAt string `json:"last_accessed_at"`
	CreatedAt      string `json:"created_at"`
}

func presentCache(row actionsdb.WorkflowCache) cacheResponse {
	return cacheResponse{
		ID:             row.ID,
		Key:            row.CacheKey,
		Version:        row.CacheVersion,
		Ref:            row.GitRef,
		SizeBytes:      row.SizeBytes,
		LastAccessedAt: row.LastAccessedAt.Time.UTC().Format(time.RFC3339),
		CreatedAt:      row.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
}

func (h *Handlers) actionsCachesList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := actionsdb.New()
	ref := pgTextOrNull(r.URL.Query().Get("ref"))
	key := pgTextOrNull(r.URL.Query().Get("key"))
	total, err := q.CountWorkflowCachesForRepo(r.Context(), h.d.Pool, actionsdb.CountWorkflowCachesForRepoParams{
		RepoID: repo.ID, GitRef: ref, CacheKey: key,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count caches", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListWorkflowCachesForRepo(r.Context(), h.d.Pool, actionsdb.ListWorkflowCachesForRepoParams{
		RepoID:   repo.ID,
		Limit:    int32(perPage),
		Offset:   int32((page - 1) * perPage),
		GitRef:   ref,
		CacheKey: key,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list caches", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]cacheResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentCache(row))
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":     total,
		"actions_caches":  out,
	})
}

func (h *Handlers) actionsCacheDeleteByID(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	cacheID, err := strconv.ParseInt(chi.URLParam(r, "cache_id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "cache not found")
		return
	}
	q := actionsdb.New()
	row, err := q.GetWorkflowCacheByID(r.Context(), h.d.Pool, cacheID)
	if err != nil || row.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "cache not found")
		return
	}
	n, err := q.DeleteWorkflowCacheByID(r.Context(), h.d.Pool, actionsdb.DeleteWorkflowCacheByIDParams{
		ID: cacheID, RepoID: repo.ID,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete cache", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if n == 0 {
		writeAPIError(w, http.StatusNotFound, "cache not found")
		return
	}
	if h.d.ObjectStore != nil {
		go h.purgeCacheObjects(context.WithoutCancel(r.Context()), []string{row.ObjectKey})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) actionsCachesDeleteByKey(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeAPIError(w, http.StatusBadRequest, "key query parameter required")
		return
	}
	ref := pgTextOrNull(r.URL.Query().Get("ref"))
	objectKeys, err := actionsdb.New().DeleteWorkflowCachesByKey(r.Context(), h.d.Pool, actionsdb.DeleteWorkflowCachesByKeyParams{
		RepoID:    repo.ID,
		CacheKey:  key,
		GitRef:    ref,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete caches by key", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if h.d.ObjectStore != nil && len(objectKeys) > 0 {
		go h.purgeCacheObjects(context.WithoutCancel(r.Context()), objectKeys)
	}
	w.WriteHeader(http.StatusNoContent)
}

// purgeCacheObjects mirrors purgeArtifactObjects but for cache
// tarballs. Detached from the request so the response returns even
// if the object-store deletes are slow; failures fall back to the
// eventual eviction sweeper (future sprint).
func (h *Handlers) purgeCacheObjects(parent context.Context, keys []string) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	for _, k := range keys {
		if err := h.d.ObjectStore.Delete(ctx, k); err != nil {
			h.d.Logger.Warn("api: purge cache object", "key", k, "error", err)
		}
	}
}

// pgTextOrNull builds a pgtype.Text whose Valid bit follows whether
// the trimmed input is empty. Maps "" → NULL parameter so the SQL
// "is null OR filter equals" pattern degenerates to "no filter".
func pgTextOrNull(s string) pgtype.Text {
	s = strings.TrimSpace(s)
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
