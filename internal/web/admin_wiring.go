// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	adminh "github.com/tenseleyFlow/shithub/internal/web/handlers/admin"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// buildAdminHandlers wires the S34 site-admin handler set. The
// returned handler set is route-only; the wiring layer in server.go
// composes RequireUser + RequireSiteAdmin around the Mount call.
//
// emailSender + branding are required for the "Reset password"
// admin action to actually deliver. Pass the same sender the
// auth handler set uses so behavior matches the public flow.
func buildAdminHandlers(
	cfg config.Config,
	pool *pgxpool.Pool,
	tmplFS fs.FS,
	logger *slog.Logger,
	version string,
	emailSender email.Sender,
) (*adminh.Handlers, error) {
	if pool == nil {
		return nil, errors.New("admin: nil pool")
	}
	rr, err := render.New(tmplFS, render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		return nil, fmt.Errorf("admin: render.New: %w", err)
	}
	return adminh.New(adminh.Deps{
		Logger: logger,
		Render: rr,
		Pool:   pool,
		Audit:  audit.NewRecorder(),
		Email:  emailSender,
		Branding: email.Branding{
			SiteName: cfg.Auth.SiteName,
			BaseURL:  cfg.Auth.BaseURL,
			From:     cfg.Auth.EmailFrom,
		},
		SiteName: cfg.Auth.SiteName,
		Version:  version,
	})
}
