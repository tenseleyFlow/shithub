// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	searchhandlers "github.com/tenseleyFlow/shithub/internal/web/handlers/search"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// buildSearchHandlers wires the S28 search handler set. Owns its own
// renderer (same pattern as the other handler builders).
//
// Limiter is the per-request rate-limit entrypoint applied to /search
// (audit 2026-05-10 H4). Pass nil from tests when you don't care
// about throttling; production wiring constructs one shared limiter
// per shithubd-web process and threads it everywhere.
func buildSearchHandlers(
	pool *pgxpool.Pool,
	tmplFS fs.FS,
	logger *slog.Logger,
	limiter *ratelimit.Limiter,
	enforce config.EnforceConfig,
) (*searchhandlers.Handlers, error) {
	rr, err := render.New(tmplFS, render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		return nil, err
	}
	return searchhandlers.New(searchhandlers.Deps{
		Logger: logger, Render: rr, Pool: pool, Limiter: limiter,
		BillingEnforce: enforce,
	})
}
