// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	searchhandlers "github.com/tenseleyFlow/shithub/internal/web/handlers/search"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// buildSearchHandlers wires the S28 search handler set. Owns its own
// renderer (same pattern as the other handler builders).
func buildSearchHandlers(
	pool *pgxpool.Pool,
	tmplFS fs.FS,
	logger *slog.Logger,
) (*searchhandlers.Handlers, error) {
	rr, err := render.New(tmplFS, render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		return nil, err
	}
	return searchhandlers.New(searchhandlers.Deps{
		Logger: logger, Render: rr, Pool: pool,
	})
}
