// SPDX-License-Identifier: AGPL-3.0-or-later

// Package api owns shithub's PAT-authenticated HTTP API surface. S08
// ships exactly one route: GET /api/v1/user. Later sprints (S22 PRs,
// S30 orgs, …) extend the surface here.
//
// Routes registered here run inside the CSRF-EXEMPT group of the chi
// router. Auth is provided by the PATAuth middleware: no PAT → 401;
// PAT lacking the required scope → 403; valid scoped PAT → 200.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// Deps is the wiring the API handlers need. Constructed by the web
// package and injected at registration time.
type Deps struct {
	Pool      *pgxpool.Pool
	Debouncer *pat.Debouncer
}

// Handlers is the registered API handler set. Construct with New.
type Handlers struct {
	d Deps
	q *usersdb.Queries
}

// New constructs the handler set, validating Deps.
func New(d Deps) (*Handlers, error) {
	if d.Pool == nil {
		return nil, errors.New("api: nil Pool")
	}
	if d.Debouncer == nil {
		d.Debouncer = pat.NewDebouncer(0)
	}
	return &Handlers{d: d, q: usersdb.New()}, nil
}

// Mount registers /api/v1/* on r. Caller is responsible for putting r
// in a CSRF-exempt group.
func (h *Handlers) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.PATAuthMiddleware(middleware.PATConfig{
			Pool:      h.d.Pool,
			Debouncer: h.d.Debouncer,
		}))
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireScope(pat.ScopeUserRead))
			r.Get("/api/v1/user", h.userMe)
		})
		// S24 check-runs / check-suites — RequireScope is per-route
		// inside the helper since reads need repo:read but writes need
		// repo:write.
		h.mountChecks(r)
	})
}

// userResponse is the public shape of a user record. Mirrors GitHub's
// /user response in spirit; we'll grow it organically as features land.
type userResponse struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name,omitempty"`
	Verified  bool   `json:"email_verified"`
	CreatedAt string `json:"created_at"`
}

func (h *Handlers) userMe(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	user, err := h.q.GetUserByID(r.Context(), h.d.Pool, auth.UserID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, userResponse{
		ID:        user.ID,
		Username:  user.Username,
		Name:      user.DisplayName,
		Verified:  user.EmailVerified,
		CreatedAt: user.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// silence unused import on the rare branch where context is not used.
var _ = context.Background
