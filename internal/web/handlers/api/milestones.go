// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountMilestones registers the S50 §3 follow-up milestones REST
// surface.
//
//	GET    /api/v1/repos/{o}/{r}/milestones[?state=]    list
//	POST   /api/v1/repos/{o}/{r}/milestones             create
//	GET    /api/v1/repos/{o}/{r}/milestones/{id}        get
//	PATCH  /api/v1/repos/{o}/{r}/milestones/{id}        update (title/description/due_on/state)
//	DELETE /api/v1/repos/{o}/{r}/milestones/{id}        delete
//
// Scope: repo:read on GETs, repo:write on mutations. Mutations gate on
// `ActionIssueLabel` — the same minimum-role gate the HTML milestone
// page enforces (write collaborator).
//
// Identifier: we expose the milestone primary-key `id` rather than a
// per-repo `number`. The schema doesn't track a number column, and
// the CLI gets the id back from the create / list responses.
func (h *Handlers) mountMilestones(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/milestones", h.milestonesList)
		r.Get("/api/v1/repos/{owner}/{repo}/milestones/{id}", h.milestoneGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/repos/{owner}/{repo}/milestones", h.milestoneCreate)
		r.Patch("/api/v1/repos/{owner}/{repo}/milestones/{id}", h.milestonePatch)
		r.Delete("/api/v1/repos/{owner}/{repo}/milestones/{id}", h.milestoneDelete)
	})
}

type milestoneResponse struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	Description  string `json:"description,omitempty"`
	State        string `json:"state"`
	DueOn        string `json:"due_on,omitempty"`
	OpenIssues   int32  `json:"open_issues"`
	ClosedIssues int32  `json:"closed_issues"`
	CreatedAt    string `json:"created_at"`
	ClosedAt     string `json:"closed_at,omitempty"`
}

func presentMilestone(m issuesdb.Milestone, open, closed int32) milestoneResponse {
	out := milestoneResponse{
		ID:           m.ID,
		Title:        m.Title,
		Description:  m.Description,
		State:        string(m.State),
		OpenIssues:   open,
		ClosedIssues: closed,
		CreatedAt:    m.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
	if m.DueOn.Valid {
		out.DueOn = m.DueOn.Time.UTC().Format(time.RFC3339)
	}
	if m.ClosedAt.Valid {
		out.ClosedAt = m.ClosedAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}

func (h *Handlers) milestonesList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	stateFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
	q := issuesdb.New()
	rows, err := q.ListMilestones(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list milestones", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]milestoneResponse, 0, len(rows))
	for _, m := range rows {
		switch stateFilter {
		case "", "all":
			// no filter
		case "open", "closed":
			if string(m.State) != stateFilter {
				continue
			}
		default:
			writeAPIError(w, http.StatusUnprocessableEntity, "state must be open, closed, or all")
			return
		}
		counts, err := q.MilestoneIssueCounts(r.Context(), h.d.Pool, pgtype.Int8{Int64: m.ID, Valid: true})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: milestone counts", "error", err, "milestone_id", m.ID)
			counts = issuesdb.MilestoneIssueCountsRow{}
		}
		out = append(out, presentMilestone(m, counts.OpenCount, counts.ClosedCount))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) milestoneGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	m, ok := h.resolveMilestone(w, r, repo.ID, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	counts, err := issuesdb.New().MilestoneIssueCounts(r.Context(), h.d.Pool, pgtype.Int8{Int64: m.ID, Valid: true})
	if err != nil {
		counts = issuesdb.MilestoneIssueCountsRow{}
	}
	writeJSON(w, http.StatusOK, presentMilestone(m, counts.OpenCount, counts.ClosedCount))
}

type milestoneCreateRequest struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	DueOn       *string `json:"due_on"`
	State       string  `json:"state"`
}

func (h *Handlers) milestoneCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	var body milestoneCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	due, err := parseMilestoneDueOn(body.DueOn)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	row, err := issues.CreateMilestone(r.Context(), h.issuesDeps(), issues.MilestoneCreateParams{
		RepoID:      repo.ID,
		Title:       body.Title,
		Description: body.Description,
		DueOn:       due,
	})
	if err != nil {
		writeMilestoneError(w, err)
		return
	}
	if body.State == "closed" {
		if err := issues.SetMilestoneState(r.Context(), h.issuesDeps(), row.ID, "closed"); err != nil {
			writeMilestoneError(w, err)
			return
		}
		row, _ = issuesdb.New().GetMilestone(r.Context(), h.d.Pool, row.ID)
	}
	writeJSON(w, http.StatusCreated, presentMilestone(row, 0, 0))
}

type milestonePatchRequest struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	DueOn       *string `json:"due_on"`
	State       *string `json:"state"`
}

func (h *Handlers) milestonePatch(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	cur, ok := h.resolveMilestone(w, r, repo.ID, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var body milestonePatchRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	title := cur.Title
	if body.Title != nil {
		title = *body.Title
	}
	desc := cur.Description
	if body.Description != nil {
		desc = *body.Description
	}
	var dueRef *time.Time
	if body.DueOn != nil {
		due, err := parseMilestoneDueOn(body.DueOn)
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		dueRef = due
	} else if cur.DueOn.Valid {
		t := cur.DueOn.Time
		dueRef = &t
	}
	if err := issues.UpdateMilestone(r.Context(), h.issuesDeps(), issues.MilestoneUpdateParams{
		ID: cur.ID, Title: title, Description: desc, DueOn: dueRef,
	}); err != nil {
		writeMilestoneError(w, err)
		return
	}
	if body.State != nil {
		newState := strings.ToLower(*body.State)
		if err := issues.SetMilestoneState(r.Context(), h.issuesDeps(), cur.ID, newState); err != nil {
			writeMilestoneError(w, err)
			return
		}
	}
	fresh, err := issuesdb.New().GetMilestone(r.Context(), h.d.Pool, cur.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	counts, _ := issuesdb.New().MilestoneIssueCounts(r.Context(), h.d.Pool, pgtype.Int8{Int64: cur.ID, Valid: true})
	writeJSON(w, http.StatusOK, presentMilestone(fresh, counts.OpenCount, counts.ClosedCount))
}

func (h *Handlers) milestoneDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	cur, ok := h.resolveMilestone(w, r, repo.ID, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if err := issues.DeleteMilestone(r.Context(), h.issuesDeps(), cur.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete milestone", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveMilestone parses the URL id, looks up the milestone, and
// rejects with 404 if it belongs to a different repo (existence-leak
// guard).
func (h *Handlers) resolveMilestone(w http.ResponseWriter, r *http.Request, repoID int64, idRaw string) (issuesdb.Milestone, bool) {
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "milestone not found")
		return issuesdb.Milestone{}, false
	}
	m, err := issuesdb.New().GetMilestone(r.Context(), h.d.Pool, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "milestone not found")
			return issuesdb.Milestone{}, false
		}
		h.d.Logger.ErrorContext(r.Context(), "api: lookup milestone", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return issuesdb.Milestone{}, false
	}
	if m.RepoID != repoID {
		writeAPIError(w, http.StatusNotFound, "milestone not found")
		return issuesdb.Milestone{}, false
	}
	return m, true
}

// parseMilestoneDueOn parses an RFC3339 timestamp. nil pointer or
// empty string clears the due_on (returns nil, nil).
func parseMilestoneDueOn(raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	s := strings.TrimSpace(*raw)
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, errors.New("due_on must be RFC3339 timestamp")
	}
	return &t, nil
}

func writeMilestoneError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, issues.ErrMilestoneExists):
		writeAPIError(w, http.StatusConflict, err.Error())
	default:
		if msg := err.Error(); strings.HasPrefix(msg, "issues: ") {
			writeAPIError(w, http.StatusUnprocessableEntity, strings.TrimPrefix(msg, "issues: "))
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}
