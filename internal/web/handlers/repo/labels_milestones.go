// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/issues"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// ─── labels ──────────────────────────────────────────────────────────

func (h *Handlers) labelsList(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	labels, _ := h.iq.ListLabels(r.Context(), h.d.Pool, row.ID)
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := viewer.PolicyActor()
	canManage := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionIssueLabel, policy.NewRepoRefFromRepo(row)).Allow
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.d.Render.RenderPage(w, r, "repo/labels", map[string]any{
		"Title":          "Labels · " + row.Name,
		"Owner":          owner.Username,
		"Repo":           row,
		"Labels":         labels,
		"CanManageIssue": canManage,
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"RepoActions":    h.repoActions(r, row.ID),
		"RepoCounts":     h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":    h.canViewSettings(viewer),
		"ActiveSubnav":   "issues",
	})
}

func (h *Handlers) labelCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	_, err := issues.CreateLabel(r.Context(), h.issuesDeps(), issues.LabelCreateParams{
		RepoID:      row.ID,
		Name:        r.PostFormValue("name"),
		Color:       r.PostFormValue("color"),
		Description: r.PostFormValue("description"),
	})
	if err != nil {
		h.handleLabelError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/labels", http.StatusSeeOther)
}

func (h *Handlers) labelUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	if err := issues.UpdateLabel(r.Context(), h.issuesDeps(), issues.LabelUpdateParams{
		ID:          id,
		Name:        r.PostFormValue("name"),
		Color:       r.PostFormValue("color"),
		Description: r.PostFormValue("description"),
	}); err != nil {
		h.handleLabelError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/labels", http.StatusSeeOther)
}

func (h *Handlers) labelDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := issues.DeleteLabel(r.Context(), h.issuesDeps(), id); err != nil {
		h.handleLabelError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/labels", http.StatusSeeOther)
}

func (h *Handlers) handleLabelError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, issues.ErrLabelExists):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "label name already taken")
	case errors.Is(err, issues.ErrLabelInvalidColor):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "label color must be 6 hex chars")
	default:
		h.d.Logger.WarnContext(r.Context(), "labels: write", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}

// ─── milestones ──────────────────────────────────────────────────────

func (h *Handlers) milestonesList(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	ms, _ := h.iq.ListMilestones(r.Context(), h.d.Pool, row.ID)
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := viewer.PolicyActor()
	canManage := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionIssueLabel, policy.NewRepoRefFromRepo(row)).Allow
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.d.Render.RenderPage(w, r, "repo/milestones", map[string]any{
		"Title":          "Milestones · " + row.Name,
		"Owner":          owner.Username,
		"Repo":           row,
		"Milestones":     ms,
		"CanManageIssue": canManage,
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"RepoActions":    h.repoActions(r, row.ID),
		"RepoCounts":     h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":    h.canViewSettings(viewer),
		"ActiveSubnav":   "issues",
	})
}

func (h *Handlers) milestoneCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	due := parseDueOn(r.PostFormValue("due_on"))
	_, err := issues.CreateMilestone(r.Context(), h.issuesDeps(), issues.MilestoneCreateParams{
		RepoID:      row.ID,
		Title:       r.PostFormValue("title"),
		Description: r.PostFormValue("description"),
		DueOn:       due,
	})
	if err != nil {
		h.handleMilestoneError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/milestones", http.StatusSeeOther)
}

func (h *Handlers) milestoneUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	due := parseDueOn(r.PostFormValue("due_on"))
	if err := issues.UpdateMilestone(r.Context(), h.issuesDeps(), issues.MilestoneUpdateParams{
		ID:          id,
		Title:       r.PostFormValue("title"),
		Description: r.PostFormValue("description"),
		DueOn:       due,
	}); err != nil {
		h.handleMilestoneError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/milestones", http.StatusSeeOther)
}

func (h *Handlers) milestoneSetState(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	state := strings.TrimSpace(r.PostFormValue("state"))
	if err := issues.SetMilestoneState(r.Context(), h.issuesDeps(), id, state); err != nil {
		h.handleMilestoneError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/milestones", http.StatusSeeOther)
}

func (h *Handlers) milestoneDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := issues.DeleteMilestone(r.Context(), h.issuesDeps(), id); err != nil {
		h.handleMilestoneError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/milestones", http.StatusSeeOther)
}

func (h *Handlers) handleMilestoneError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, issues.ErrMilestoneExists):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "milestone title already taken")
	default:
		h.d.Logger.WarnContext(r.Context(), "milestones: write", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}

// parseDueOn accepts a yyyy-mm-dd input from the form (HTML date input
// shape). Empty string clears the due date. Anything unparseable is
// treated as cleared so a malformed date doesn't 400 the form.
func parseDueOn(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil
	}
	return &t
}
