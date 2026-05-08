// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/checks"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountChecks registers the S24 check-runs / check-suites routes.
// Caller has already wrapped this group with PATAuthMiddleware; we
// add a per-route RequireScope(repo:write or repo:read) on top.
func (h *Handlers) mountChecks(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/repos/{owner}/{repo}/check-runs", h.checkRunCreate)
		r.Patch("/api/v1/repos/{owner}/{repo}/check-runs/{id}", h.checkRunUpdate)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/commits/{sha}/check-runs", h.checkRunsForCommit)
		r.Get("/api/v1/repos/{owner}/{repo}/commits/{sha}/check-suites", h.checkSuitesForCommit)
	})
}

// resolveRepo loads the repo for {owner}/{repo} and confirms the
// PAT-authenticated user has the `action` permission. Returns the
// resolved repo (or nil, false on failure with response written).
func (h *Handlers) resolveAPIRepo(w http.ResponseWriter, r *http.Request, action policy.Action) (*reposdb.Repo, bool) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return nil, false
	}
	owner, err := usersdb.New().GetUserByUsername(r.Context(), h.d.Pool, chi.URLParam(r, "owner"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "repo not found")
		return nil, false
	}
	repo, err := reposdb.New().GetRepoByOwnerUserAndName(r.Context(), h.d.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
		OwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:        chi.URLParam(r, "repo"),
	})
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "repo not found")
		return nil, false
	}
	actor := policy.UserActor(auth.UserID, "", false, false)
	if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, action, policy.NewRepoRefFromRepo(repo)).Allow {
		// Existence-leak: 404 instead of 403 when the actor can't see
		// the repo. The PAT-scope check above is the public 403; this
		// is the visibility gate.
		writeAPIError(w, http.StatusNotFound, "repo not found")
		return nil, false
	}
	return &repo, true
}

// ─── POST /api/v1/repos/{owner}/{repo}/check-runs ───────────────────

type checkRunRequest struct {
	Name        string        `json:"name"`
	HeadSHA     string        `json:"head_sha"`
	AppSlug     string        `json:"app_slug,omitempty"`
	Status      string        `json:"status,omitempty"`
	Conclusion  string        `json:"conclusion,omitempty"`
	StartedAt   string        `json:"started_at,omitempty"`
	CompletedAt string        `json:"completed_at,omitempty"`
	DetailsURL  string        `json:"details_url,omitempty"`
	Output      checks.Output `json:"output,omitempty"`
	ExternalID  string        `json:"external_id,omitempty"`
}

func (h *Handlers) checkRunCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	var body checkRunRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	startedAt, err := parseTimeOptional(body.StartedAt)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "started_at: "+err.Error())
		return
	}
	completedAt, err := parseTimeOptional(body.CompletedAt)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "completed_at: "+err.Error())
		return
	}
	run, err := checks.Create(r.Context(), checks.Deps{Pool: h.d.Pool}, checks.CreateParams{
		RepoID:      repo.ID,
		HeadSHA:     body.HeadSHA,
		AppSlug:     body.AppSlug,
		Name:        body.Name,
		Status:      body.Status,
		Conclusion:  body.Conclusion,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		DetailsURL:  body.DetailsURL,
		Output:      body.Output,
		ExternalID:  body.ExternalID,
	})
	if err != nil {
		writeChecksError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, presentRun(run))
}

// ─── PATCH /api/v1/repos/{owner}/{repo}/check-runs/{id} ─────────────

type checkRunUpdateRequest struct {
	Status      *string        `json:"status,omitempty"`
	Conclusion  *string        `json:"conclusion,omitempty"`
	StartedAt   *string        `json:"started_at,omitempty"`
	CompletedAt *string        `json:"completed_at,omitempty"`
	DetailsURL  *string        `json:"details_url,omitempty"`
	Output      *checks.Output `json:"output,omitempty"`
}

func (h *Handlers) checkRunUpdate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	// Ensure the run belongs to this repo before applying.
	cur, err := checksdb.New().GetCheckRun(r.Context(), h.d.Pool, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "run not found")
		} else {
			writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		}
		return
	}
	if cur.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	var body checkRunUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	params := checks.UpdateParams{RunID: id}
	if body.Status != nil {
		params.HasStatus = true
		params.Status = *body.Status
	}
	if body.Conclusion != nil {
		params.HasConclusion = true
		params.Conclusion = *body.Conclusion
	}
	if body.StartedAt != nil {
		t, err := parseTimeOptional(*body.StartedAt)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "started_at: "+err.Error())
			return
		}
		params.HasStartedAt = true
		params.StartedAt = t
	}
	if body.CompletedAt != nil {
		t, err := parseTimeOptional(*body.CompletedAt)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "completed_at: "+err.Error())
			return
		}
		params.HasCompletedAt = true
		params.CompletedAt = t
	}
	if body.DetailsURL != nil {
		params.HasDetailsURL = true
		params.DetailsURL = *body.DetailsURL
	}
	if body.Output != nil {
		params.HasOutput = true
		params.Output = *body.Output
	}
	updated, err := checks.Update(r.Context(), checks.Deps{Pool: h.d.Pool}, params)
	if err != nil {
		writeChecksError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, presentRun(updated))
}

// ─── GET /api/v1/repos/{owner}/{repo}/commits/{sha}/check-runs ──────

func (h *Handlers) checkRunsForCommit(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	sha := chi.URLParam(r, "sha")
	rows, err := checksdb.New().ListCheckRunsForCommit(r.Context(), h.d.Pool, checksdb.ListCheckRunsForCommitParams{
		RepoID: repo.ID, HeadSha: sha,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, run := range rows {
		out = append(out, presentRun(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// ─── GET /api/v1/repos/{owner}/{repo}/commits/{sha}/check-suites ────

func (h *Handlers) checkSuitesForCommit(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	sha := chi.URLParam(r, "sha")
	rows, err := checksdb.New().ListCheckSuitesForCommit(r.Context(), h.d.Pool, checksdb.ListCheckSuitesForCommitParams{
		RepoID: repo.ID, HeadSha: sha,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, presentSuite(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"suites": out})
}

// ─── helpers ────────────────────────────────────────────────────────

func parseTimeOptional(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

func presentRun(r checksdb.CheckRun) map[string]any {
	out := map[string]any{
		"id":          r.ID,
		"suite_id":    r.SuiteID,
		"head_sha":    r.HeadSha,
		"name":        r.Name,
		"status":      string(r.Status),
		"details_url": r.DetailsUrl,
		"output":      json.RawMessage(r.Output),
	}
	if r.Conclusion.Valid {
		out["conclusion"] = string(r.Conclusion.CheckConclusion)
	}
	if r.StartedAt.Valid {
		out["started_at"] = r.StartedAt.Time.Format(time.RFC3339)
	}
	if r.CompletedAt.Valid {
		out["completed_at"] = r.CompletedAt.Time.Format(time.RFC3339)
	}
	if r.ExternalID.Valid {
		out["external_id"] = r.ExternalID.String
	}
	return out
}

func presentSuite(s checksdb.CheckSuite) map[string]any {
	out := map[string]any{
		"id":        s.ID,
		"head_sha":  s.HeadSha,
		"app_slug":  s.AppSlug,
		"status":    string(s.Status),
	}
	if s.Conclusion.Valid {
		out["conclusion"] = string(s.Conclusion.CheckConclusion)
	}
	return out
}

// writeChecksError maps the orchestrator's typed errors to HTTP codes.
func writeChecksError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, checks.ErrEmptyName),
		errors.Is(err, checks.ErrNameTooLong),
		errors.Is(err, checks.ErrInvalidStatus),
		errors.Is(err, checks.ErrInvalidConclusion),
		errors.Is(err, checks.ErrCompletedNeedsConclusion),
		errors.Is(err, checks.ErrShortHeadSHA),
		errors.Is(err, checks.ErrOutputTextTooLarge),
		errors.Is(err, checks.ErrOutputSummaryTooLarge):
		writeAPIError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, checks.ErrCheckRunNotFound),
		errors.Is(err, checks.ErrSuiteNotFound):
		writeAPIError(w, http.StatusNotFound, err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}
