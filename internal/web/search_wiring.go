// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	searchhandlers "github.com/tenseleyFlow/shithub/internal/web/handlers/search"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// buildSearchHandlers wires the S28 search handler set. Takes the
// process-wide shared renderer (see server.go) — handler builders must
// never construct their own.
//
// Limiter is the per-request rate-limit entrypoint applied to /search
// (audit 2026-05-10 H4). Pass nil from tests when you don't care
// about throttling; production wiring constructs one shared limiter
// per shithubd-web process and threads it everywhere.
func buildSearchHandlers(
	pool *pgxpool.Pool,
	rr *render.Renderer,
	logger *slog.Logger,
	limiter *ratelimit.Limiter,
	enforce config.EnforceConfig,
) (*searchhandlers.Handlers, error) {
	if rr == nil {
		return nil, errors.New("search: nil renderer")
	}
	return searchhandlers.New(searchhandlers.Deps{
		Logger: logger, Render: rr, Pool: pool, Limiter: limiter,
		BillingEnforce: enforce,
	})
}
