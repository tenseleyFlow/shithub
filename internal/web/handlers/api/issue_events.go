// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountIssueEvents registers the S50 §20 issue-events / timeline surface.
//
//	GET /api/v1/repos/{owner}/{repo}/issues/{number}/events
//
// Read-only. Scope: `repo:read`. The timeline is exactly the
// `issue_events` rows recorded by every state mutation (labeled,
// unlabeled, milestoned, demilestoned, locked, unlocked, closed,
// reopened, referenced, …) — the same audit trail the HTML issue
// page renders. Paginated with standard `Link:` headers; events are
// sorted oldest-first to match gh and the HTML view.
//
// We do not roll comments into the timeline yet (that's the
// `/timeline` endpoint, distinct from `/events` on gh — separate
// follow-up).
func (h *Handlers) mountIssueEvents(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/issues/{number}/events", h.issueEventsList)
	})
}

type issueEventResponse struct {
	ID            int64           `json:"id"`
	Kind          string          `json:"kind"`
	ActorUserID   int64           `json:"actor_user_id,omitempty"`
	ActorUsername string          `json:"actor_username,omitempty"`
	Meta          json.RawMessage `json:"meta,omitempty"`
	RefTargetID   int64           `json:"ref_target_id,omitempty"`
	CreatedAt     string          `json:"created_at"`
}

func (h *Handlers) issueEventsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	issue, ok := h.resolveIssueOrPRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := issuesdb.New()
	total, err := q.CountIssueEvents(r.Context(), h.d.Pool, issue.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count issue events", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListIssueEventsWithActor(r.Context(), h.d.Pool, issuesdb.ListIssueEventsWithActorParams{
		IssueID: issue.ID,
		Limit:   int32(perPage),
		Offset:  int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list issue events", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]issueEventResponse, 0, len(rows))
	for _, row := range rows {
		ev := issueEventResponse{
			ID:        row.ID,
			Kind:      row.Kind,
			CreatedAt: row.CreatedAt.Time.UTC().Format(time.RFC3339),
		}
		if row.ActorUserID.Valid {
			ev.ActorUserID = row.ActorUserID.Int64
		}
		if row.ActorUsername.Valid {
			ev.ActorUsername = row.ActorUsername.String
		}
		if row.RefTargetID.Valid {
			ev.RefTargetID = row.RefTargetID.Int64
		}
		if len(row.Meta) > 0 {
			ev.Meta = json.RawMessage(row.Meta)
		}
		out = append(out, ev)
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}
