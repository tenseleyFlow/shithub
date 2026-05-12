// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
)

// Deps wires lifecycle operations to postgres and optional runtime services.
type Deps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Logger *slog.Logger
}
