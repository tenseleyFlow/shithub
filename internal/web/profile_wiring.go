// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	profileh "github.com/tenseleyFlow/shithub/internal/web/handlers/profile"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// buildProfileHandlers constructs the read-only profile handler set.
// objectStore may be nil — handlers fall back to the identicon path.
func buildProfileHandlers(
	pool *pgxpool.Pool, objectStore storage.ObjectStore, tmplFS fs.FS,
	logger *slog.Logger,
) (*profileh.Handlers, error) {
	rr, err := render.New(tmplFS, render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		return nil, err
	}
	return profileh.New(profileh.Deps{
		Logger:      logger,
		Render:      rr,
		Pool:        pool,
		ObjectStore: objectStore,
	})
}
