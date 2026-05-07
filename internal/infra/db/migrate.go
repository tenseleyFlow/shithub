// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// MigrationsFS holds the embedded migrations. The web/cmd packages set this
// at init time via SetMigrationsFS(); we don't embed here because embed
// directives can't traverse upward to the migrations/ directory at the repo
// root.
var migrationsFS fs.FS

// SetMigrationsFS registers the migrations filesystem. The repo's
// `internal/migrationsfs` package embeds and registers it; tests can swap
// it for a fixture FS.
func SetMigrationsFS(fsys fs.FS) {
	migrationsFS = fsys
}

// MigrateAction is one of the migrate operations the CLI exposes.
type MigrateAction string

const (
	MigrateUp      MigrateAction = "up"
	MigrateDown    MigrateAction = "down"
	MigrateStatus  MigrateAction = "status"
	MigrateVersion MigrateAction = "version"
	MigrateRedo    MigrateAction = "redo"
	MigrateReset   MigrateAction = "reset"
)

// Migrate runs the requested goose action against the pool's underlying
// database. We reuse the pool's connection config rather than opening a
// fresh sql.DB from scratch, so the same DSN env-var-driven configuration
// applies to migrations.
//
// goose requires `database/sql`. We bridge from pgx's connection config
// using `pgx/v5/stdlib`.
func Migrate(ctx context.Context, cfg Config, action MigrateAction) error {
	if migrationsFS == nil {
		return errors.New("db: migrationsFS not registered (call SetMigrationsFS)")
	}
	cfg = cfg.Resolve()
	if cfg.URL == "" {
		return ErrNoURL
	}

	connCfg, err := pgx.ParseConfig(cfg.URL)
	if err != nil {
		return fmt.Errorf("db: parse migrate URL: %w", err)
	}
	sqldb := stdlib.OpenDB(*connCfg)
	defer func() { _ = sqldb.Close() }()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("db: goose dialect: %w", err)
	}
	goose.SetBaseFS(migrationsFS)

	switch action {
	case MigrateUp:
		return goose.UpContext(ctx, sqldb, ".")
	case MigrateDown:
		return goose.DownContext(ctx, sqldb, ".")
	case MigrateStatus:
		return goose.StatusContext(ctx, sqldb, ".")
	case MigrateVersion:
		v, err := goose.GetDBVersionContext(ctx, sqldb)
		if err != nil {
			return fmt.Errorf("db: version: %w", err)
		}
		fmt.Printf("schema version: %d\n", v)
		return nil
	case MigrateRedo:
		return goose.RedoContext(ctx, sqldb, ".")
	case MigrateReset:
		return goose.ResetContext(ctx, sqldb, ".")
	default:
		return fmt.Errorf("db: unknown migrate action %q", action)
	}
}
