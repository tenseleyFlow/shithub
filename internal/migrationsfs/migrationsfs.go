// SPDX-License-Identifier: AGPL-3.0-or-later

// Package migrationsfs embeds the SQL migrations from the repo root and
// registers them with the db package. It exists as its own package because
// //go:embed paths can't traverse upward; the migrationsfs/ directory is at
// the right level relative to migrations/ to embed it.
package migrationsfs

import (
	"embed"
	"io/fs"

	"github.com/tenseleyFlow/shithub/internal/infra/db"
)

//go:embed all:migrations
var migrationsFS embed.FS

// FS returns the embedded migrations filesystem rooted at "migrations/".
func FS() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

func init() {
	db.SetMigrationsFS(FS())
}
