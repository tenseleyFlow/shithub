// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/actions/dispatch"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountActionsLifecycleREST registers the §13 part 2 lifecycle
// surface: workflow enable/disable, run delete, artifact list +
// download + delete, and job-log download.
//
// Cancel + rerun live on the existing mountActionsLifecycle (S41g)
// routes and are not duplicated here.
//
// Scopes: `repo:read` on the GETs, `repo:write` on the mutations.
func (h *Handlers) mountActionsLifecycleREST(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/actions/runs/{run_id}/artifacts", h.actionsRunArtifactsList)
		r.Get("/api/v1/repos/{owner}/{repo}/actions/artifacts/{artifact_id}", h.actionsArtifactGet)
		r.Get("/api/v1/repos/{owner}/{repo}/actions/artifacts/{artifact_id}/zip", h.actionsArtifactDownload)
		r.Get("/api/v1/repos/{owner}/{repo}/actions/jobs/{job_id}/logs", h.actionsJobLogs)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Put("/api/v1/repos/{owner}/{repo}/actions/workflows/{workflow}/enable", h.actionsWorkflowEnable)
		r.Put("/api/v1/repos/{owner}/{repo}/actions/workflows/{workflow}/disable", h.actionsWorkflowDisable)
		r.Delete("/api/v1/repos/{owner}/{repo}/actions/runs/{run_id}", h.actionsRunDelete)
		r.Delete("/api/v1/repos/{owner}/{repo}/actions/artifacts/{artifact_id}", h.actionsArtifactDelete)
	})
}

// ─── workflow enable / disable ──────────────────────────────────────

func (h *Handlers) actionsWorkflowEnable(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	file, err := dispatch.NormalizeFilePath(chi.URLParam(r, "workflow"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid workflow file path")
		return
	}
	if _, err := actionsdb.New().EnableWorkflow(r.Context(), h.d.Pool, actionsdb.EnableWorkflowParams{
		RepoID: repo.ID, WorkflowFile: file,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: workflow enable", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "enable failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) actionsWorkflowDisable(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	file, err := dispatch.NormalizeFilePath(chi.URLParam(r, "workflow"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid workflow file path")
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if err := actionsdb.New().DisableWorkflow(r.Context(), h.d.Pool, actionsdb.DisableWorkflowParams{
		RepoID:           repo.ID,
		WorkflowFile:     file,
		DisabledByUserID: pgtype.Int8{Int64: auth.UserID, Valid: auth.UserID != 0},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: workflow disable", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "disable failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── run delete ─────────────────────────────────────────────────────

func (h *Handlers) actionsRunDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	runID, err := strconv.ParseInt(chi.URLParam(r, "run_id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	q := actionsdb.New()
	run, err := q.GetWorkflowRunByID(r.Context(), h.d.Pool, runID)
	if err != nil || run.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	objectKeys, err := q.ListArtifactObjectKeysForRun(r.Context(), h.d.Pool, runID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list run artifact keys", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if _, err := q.DeleteWorkflowRunByID(r.Context(), h.d.Pool, runID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete run", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if h.d.ObjectStore != nil && len(objectKeys) > 0 {
		go h.purgeArtifactObjects(objectKeys)
	}
	w.WriteHeader(http.StatusNoContent)
}

// purgeArtifactObjects is a best-effort S3 cleanup detached from the
// request lifecycle. Failures are logged but never surfaced — the
// authoritative DB row is gone, and the cleanup sweeper retries
// orphan-object deletion on its own schedule.
func (h *Handlers) purgeArtifactObjects(keys []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, k := range keys {
		if err := h.d.ObjectStore.Delete(ctx, k); err != nil {
			h.d.Logger.Warn("api: purge artifact object", "key", k, "error", err)
		}
	}
}

// ─── run artifacts ──────────────────────────────────────────────────

type artifactResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	SizeBytes  int64  `json:"size_bytes"`
	ArchiveURL string `json:"archive_url"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	CreatedAt  string `json:"created_at"`
}

func (h *Handlers) actionsRunArtifactsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	runID, err := strconv.ParseInt(chi.URLParam(r, "run_id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	q := actionsdb.New()
	run, err := q.GetWorkflowRunByID(r.Context(), h.d.Pool, runID)
	if err != nil || run.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	rows, err := q.ListArtifactsForRun(r.Context(), h.d.Pool, runID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list artifacts", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]artifactResponse, 0, len(rows))
	for _, a := range rows {
		out = append(out, artifactResponse{
			ID:         a.ID,
			Name:       a.Name,
			SizeBytes:  a.ByteCount,
			ArchiveURL: artifactArchiveURL(r, chi.URLParam(r, "owner"), repo.Name,a.ID),
			ExpiresAt:  pgTimestampString(a.ExpiresAt),
			CreatedAt:  a.CreatedAt.Time.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) actionsArtifactGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	artifact, ok := h.lookupArtifact(w, r, repo.ID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, artifactResponse{
		ID:         artifact.ID,
		Name:       artifact.Name,
		SizeBytes:  artifact.ByteCount,
		ArchiveURL: artifactArchiveURL(r, chi.URLParam(r, "owner"), repo.Name,artifact.ID),
		ExpiresAt:  pgTimestampString(artifact.ExpiresAt),
		CreatedAt:  artifact.CreatedAt.Time.UTC().Format(time.RFC3339),
	})
}

func (h *Handlers) actionsArtifactDownload(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	artifact, ok := h.lookupArtifact(w, r, repo.ID)
	if !ok {
		return
	}
	if h.d.ObjectStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "object store not configured")
		return
	}
	rc, _, err := h.d.ObjectStore.Get(r.Context(), artifact.ObjectKey)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: artifact get", "error", err, "key", artifact.ObjectKey)
		writeAPIError(w, http.StatusNotFound, "artifact blob not found")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+artifact.Name+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, rc); err != nil {
		h.d.Logger.WarnContext(r.Context(), "api: artifact stream", "error", err)
	}
}

func (h *Handlers) actionsArtifactDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	artifact, ok := h.lookupArtifact(w, r, repo.ID)
	if !ok {
		return
	}
	if _, err := actionsdb.New().DeleteWorkflowArtifactByID(r.Context(), h.d.Pool, artifact.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete artifact", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if h.d.ObjectStore != nil {
		go h.purgeArtifactObjects([]string{artifact.ObjectKey})
	}
	w.WriteHeader(http.StatusNoContent)
}

// lookupArtifact resolves the {artifact_id} URL param against the
// repo so a caller can't drive `/repos/foo/bar/actions/artifacts/<id>`
// against an artifact in an unrelated repo.
func (h *Handlers) lookupArtifact(w http.ResponseWriter, r *http.Request, repoID int64) (actionsdb.WorkflowArtifact, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "artifact_id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "artifact not found")
		return actionsdb.WorkflowArtifact{}, false
	}
	q := actionsdb.New()
	artifact, err := q.GetArtifactByID(r.Context(), h.d.Pool, id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "artifact not found")
		return actionsdb.WorkflowArtifact{}, false
	}
	run, err := q.GetWorkflowRunByID(r.Context(), h.d.Pool, artifact.RunID)
	if err != nil || run.RepoID != repoID {
		writeAPIError(w, http.StatusNotFound, "artifact not found")
		return actionsdb.WorkflowArtifact{}, false
	}
	return artifact, true
}

// ─── job logs ───────────────────────────────────────────────────────

func (h *Handlers) actionsJobLogs(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	jobID, err := strconv.ParseInt(chi.URLParam(r, "job_id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return
	}
	q := actionsdb.New()
	job, err := q.GetWorkflowJobByID(r.Context(), h.d.Pool, jobID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return
	}
	run, err := q.GetWorkflowRunByID(r.Context(), h.d.Pool, job.RunID)
	if err != nil || run.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return
	}
	steps, err := q.ListStepsForJob(r.Context(), h.d.Pool, jobID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list steps", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "logs unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	for _, step := range steps {
		// One small header per step keeps the concatenated transcript
		// scannable; gh's job-logs API does the same.
		if _, werr := io.WriteString(w, "##[group] step "+strconv.FormatInt(int64(step.StepIndex), 10)+": "+step.StepName+"\n"); werr != nil {
			return
		}
		chunks, err := q.ListAllStepLogChunksForStep(r.Context(), h.d.Pool, step.ID)
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "api: list log chunks", "error", err, "step_id", step.ID)
			continue
		}
		for _, c := range chunks {
			if _, werr := w.Write(c.Chunk); werr != nil {
				return
			}
		}
		if _, werr := io.WriteString(w, "##[endgroup]\n"); werr != nil {
			return
		}
	}
}

// ─── helpers ────────────────────────────────────────────────────────

func artifactArchiveURL(r *http.Request, owner, repo string, artifactID int64) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/v1/repos/" + owner + "/" + repo + "/actions/artifacts/" + strconv.FormatInt(artifactID, 10) + "/zip"
}

func pgTimestampString(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}
