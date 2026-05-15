// SPDX-License-Identifier: AGPL-3.0-or-later

package migrationsfs_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/tenseleyFlow/shithub/internal/migrationsfs"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// PRO-EXT_SR-03: pin that migration 0094's Down completes cleanly
// even when user-scope rows exist in workflow_secrets or
// actions_variables. Before this sprint, the Down re-applied a
// tighter 2-way XOR constraint *before* dropping the user_id column,
// which failed on any row with user_id IS NOT NULL.

func TestMigration0094_DownWithUserScopeRows(t *testing.T) {
	pool := dbtest.NewTestDB(t)

	// Seed a user-scope secret and a user-scope variable. The template
	// has all migrations applied, so the user_id columns + 3-way XOR
	// are already in place.
	ctx := context.Background()
	var userID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, session_epoch)
		VALUES ('alice-sr03', '!', 1) RETURNING id
	`).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_secrets (user_id, name, ciphertext, nonce)
		VALUES ($1, 'SR03_S', '\x00010203'::bytea, '\x000102030405060708090a0b'::bytea)
	`, userID); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO actions_variables (user_id, name, value)
		VALUES ($1, 'SR03_V', 'sr03-value')
	`, userID); err != nil {
		t.Fatalf("seed variable: %v", err)
	}

	// Open a sql.DB on the same DSN. goose needs database/sql.
	connCfg := pool.Config().ConnConfig
	sqldb := stdlib.OpenDB(*connCfg)
	defer func() { _ = sqldb.Close() }()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	goose.SetBaseFS(migrationsfs.FS())

	// Roll back ONE migration (this is migration 95+ on the template,
	// so DownContext reverts the most recent — which is whatever is
	// latest at the time this test was written. We want to specifically
	// target 0094, so use DownTo with the version below.
	if err := goose.DownToContext(ctx, sqldb, ".", 93); err != nil {
		t.Fatalf("goose DownTo 93: %v", err)
	}

	// Confirm the user_id column is gone — the schema reverted.
	var hasCol bool
	if err := sqldb.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'workflow_secrets' AND column_name = 'user_id'
		)
	`).Scan(&hasCol); err != nil {
		t.Fatalf("check column: %v", err)
	}
	if hasCol {
		t.Errorf("workflow_secrets.user_id still present after Down")
	}

	// Roll back further migrations could fail (they're not what we're
	// testing). Restore by re-applying up to head so the test pool's
	// cleanup hooks don't choke on a partial schema.
	if err := goose.UpContext(ctx, sqldb, "."); err != nil {
		t.Fatalf("goose Up restore: %v", err)
	}
}

// Static compile assertion that the imports stay live even if pgx's
// stdlib hook changes.
var (
	_ = pgx.ParseConfig
	_ sql.DB
)
