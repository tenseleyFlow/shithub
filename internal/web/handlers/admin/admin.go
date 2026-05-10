// SPDX-License-Identifier: AGPL-3.0-or-later

// Package admin owns the /admin/* surface — the operator dashboard
// for users/orgs/repos/jobs/audit/system management. Every route is
// gated by RequireSiteAdmin; non-admins receive 404 (existence-leak
// guard) per the S34 spec.
package admin

import (
	"errors"
	"log/slog"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	admindb "github.com/tenseleyFlow/shithub/internal/admin/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps wires the admin handler set.
type Deps struct {
	Logger *slog.Logger
	Render *render.Renderer
	Pool   *pgxpool.Pool
	Audit  *audit.Recorder
	// Email + Branding power the admin "Reset password" send. Required:
	// without them userResetPassword mints a token and never delivers
	// it (SR2 C3 — pre-fix the audit row falsely claimed "sent").
	Email    email.Sender
	Branding email.Branding
	// SiteName is rendered into pages; matches Auth.SiteName from config.
	SiteName string
	// Version is the running shithubd version, surfaced on /admin/system.
	Version string
}

// Handlers is the admin handler set.
type Handlers struct {
	d  Deps
	aq *admindb.Queries
	uq *usersdb.Queries
}

// New constructs the admin handler set.
func New(d Deps) (*Handlers, error) {
	if d.Render == nil {
		return nil, errors.New("admin handlers: nil Render")
	}
	if d.Pool == nil {
		return nil, errors.New("admin handlers: nil Pool")
	}
	if d.Audit == nil {
		d.Audit = audit.NewRecorder()
	}
	return &Handlers{d: d, aq: admindb.New(), uq: usersdb.New()}, nil
}

// Mount registers every /admin/* route. Caller wraps the whole set in
// RequireUser + RequireSiteAdmin so this method only registers paths
// and lets the middleware enforce.
func (h *Handlers) Mount(r chi.Router) {
	r.Get("/admin", h.dashboard)

	// Users
	r.Get("/admin/users", h.usersList)
	r.Get("/admin/users/{id}", h.userView)
	r.Post("/admin/users/{id}/suspend", h.userSuspend)
	r.Post("/admin/users/{id}/unsuspend", h.userUnsuspend)
	r.Post("/admin/users/{id}/site-admin", h.userToggleSiteAdmin)
	r.Post("/admin/users/{id}/reset-password", h.userResetPassword)

	// Repos
	r.Get("/admin/repos", h.reposList)
	r.Get("/admin/repos/{id}", h.repoView)
	r.Post("/admin/repos/{id}/archive", h.repoForceArchive)
	r.Post("/admin/repos/{id}/delete", h.repoForceDelete)

	// Jobs
	r.Get("/admin/jobs", h.jobsList)
	r.Get("/admin/jobs/{id}", h.jobView)
	r.Post("/admin/jobs/{id}/retry", h.jobRetry)
	r.Post("/admin/jobs/{id}/discard", h.jobDiscard)

	// Audit
	r.Get("/admin/audit", h.auditList)

	// System / config
	r.Get("/admin/system", h.systemPage)
	r.Get("/admin/email", h.emailQueue)

	// Impersonation
	r.Post("/admin/impersonate/{id}", h.impersonateStart)
	r.Post("/admin/impersonate/stop", h.impersonateStop)
	r.Post("/admin/impersonate/write-mode", h.impersonateEnableWrites)
}
