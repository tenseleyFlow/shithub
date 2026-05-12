// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountEvents registers the S50 §16 events REST surface.
//
//	GET /api/v1/repos/{o}/{r}/events[?page=&per_page=]   repo activity feed
//	GET /api/v1/users/{username}/events[?page=&per_page=] user public activity
//
// The repo feed gates on `ActionRepoRead` (so private-repo events
// stay invisible to non-collaborators) and returns every event the
// caller can see. The user feed only returns rows flagged
// `public=true` — matches gh, which never surfaces private-repo
// activity on a user's events feed.
//
// Scope: `repo:read` on the repo feed (the caller needs at least
// repo-level read), `user:read` on the user feed.
func (h *Handlers) mountEvents(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/events", h.repoEventsList)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/users/{username}/events", h.userEventsList)
	})
}

type eventResponse struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Public    bool            `json:"public"`
	ActorID   int64           `json:"actor_id,omitempty"`
	RepoID    int64           `json:"repo_id,omitempty"`
	Source    eventSource     `json:"source"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt string          `json:"created_at"`
}

type eventSource struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
}

func presentEvent(ev socialdb.DomainEvent) eventResponse {
	out := eventResponse{
		ID:        ev.ID,
		Kind:      ev.Kind,
		Public:    ev.Public,
		Source:    eventSource{Kind: ev.SourceKind, ID: ev.SourceID},
		CreatedAt: ev.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
	if ev.ActorUserID.Valid {
		out.ActorID = ev.ActorUserID.Int64
	}
	if ev.RepoID.Valid {
		out.RepoID = ev.RepoID.Int64
	}
	if len(ev.Payload) > 0 {
		out.Payload = json.RawMessage(ev.Payload)
	}
	return out
}

func (h *Handlers) repoEventsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	rows, err := socialdb.New().ListEventsForRepo(r.Context(), h.d.Pool, socialdb.ListEventsForRepoParams{
		RepoID: pgtype.Int8{Int64: repo.ID, Valid: true},
		Limit:  int32(perPage),
		Offset: int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list repo events", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]eventResponse, 0, len(rows))
	for _, ev := range rows {
		out = append(out, presentEvent(ev))
	}
	// We don't have a cheap O(1) count of repo events (the table is
	// append-only and high-churn), so emit `next`/`prev` only when
	// the current page filled — same pattern as commits.
	hasMore := len(rows) == perPage
	link := apipage.Page{Current: page, PerPage: perPage, Total: -1, HasMore: hasMore}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) userEventsList(w http.ResponseWriter, r *http.Request) {
	user, err := usersdb.New().GetUserByUsername(r.Context(), h.d.Pool, chi.URLParam(r, "username"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "user not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: lookup user", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	rows, err := socialdb.New().ListPublicEventsForActor(r.Context(), h.d.Pool, socialdb.ListPublicEventsForActorParams{
		ActorUserID: pgtype.Int8{Int64: user.ID, Valid: true},
		Limit:       int32(perPage),
		Offset:      int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list user events", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]eventResponse, 0, len(rows))
	for _, ev := range rows {
		out = append(out, presentEvent(ev))
	}
	hasMore := len(rows) == perPage
	link := apipage.Page{Current: page, PerPage: perPage, Total: -1, HasMore: hasMore}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}
