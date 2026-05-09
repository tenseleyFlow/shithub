// SPDX-License-Identifier: AGPL-3.0-or-later

package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	admindb "github.com/tenseleyFlow/shithub/internal/admin/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// jobsList renders the queue inspector with optional kind + status filters.
func (h *Handlers) jobsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := strings.TrimSpace(q.Get("kind"))
	status := strings.TrimSpace(q.Get("status"))
	const perPage = 100
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	rows, _ := h.aq.ListJobsForAdmin(r.Context(), h.d.Pool, admindb.ListJobsForAdminParams{
		Kind:         pgtype.Text{String: kind, Valid: kind != ""},
		StatusFilter: pgtype.Text{String: status, Valid: status != ""},
		Limit:        perPage,
		Offset:       int32((page - 1) * perPage),
	})
	counts, _ := h.aq.CountJobsByStatus(r.Context(), h.d.Pool)
	h.d.Render.RenderPage(w, r, "admin/jobs_list", map[string]any{
		"Title":       "Jobs",
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
		"AdminActive": "jobs",
		"Jobs":        rows,
		"Kind":        kind,
		"Status":      status,
		"Counts":      counts,
		"Page":        page,
		"NextPage":    page + 1,
		"PrevPage":    page - 1,
		"HasMore":     len(rows) == perPage,
		"Notice":      adminNotice(q.Get("notice")),
	})
}

// jobView renders a single job's full payload + last_error.
func (h *Handlers) jobView(w http.ResponseWriter, r *http.Request) {
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	h.d.Render.RenderPage(w, r, "admin/job_view", map[string]any{
		"Title":       "Job #" + strconv.FormatInt(job.ID, 10),
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
		"AdminActive": "jobs",
		"Job":         job,
		"PayloadStr":  string(job.Payload),
	})
}

// jobRetry re-arms a failed/dead job for immediate dispatch.
func (h *Handlers) jobRetry(w http.ResponseWriter, r *http.Request) {
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	if err := h.aq.AdminRetryJob(r.Context(), h.d.Pool, job.ID); err != nil {
		http.Error(w, "retry failed", http.StatusInternalServerError)
		return
	}
	h.recordAdminAction(r, audit.ActionAdminJobRetried, audit.Target("job"), job.ID,
		map[string]any{"kind": job.Kind})
	http.Redirect(w, r, "/admin/jobs/"+strconv.FormatInt(job.ID, 10)+"?notice=saved", http.StatusSeeOther)
}

// jobDiscard marks a job dead without running it.
func (h *Handlers) jobDiscard(w http.ResponseWriter, r *http.Request) {
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	if err := h.aq.AdminDiscardJob(r.Context(), h.d.Pool, job.ID); err != nil {
		http.Error(w, "discard failed", http.StatusInternalServerError)
		return
	}
	h.recordAdminAction(r, audit.ActionAdminJobDiscarded, audit.Target("job"), job.ID,
		map[string]any{"kind": job.Kind})
	http.Redirect(w, r, "/admin/jobs?notice=saved", http.StatusSeeOther)
}

// loadJob parses {id} and fetches.
func (h *Handlers) loadJob(w http.ResponseWriter, r *http.Request) (admindb.Job, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return admindb.Job{}, false
	}
	job, err := h.aq.GetJobForAdmin(r.Context(), h.d.Pool, id)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return admindb.Job{}, false
	}
	return job, true
}
