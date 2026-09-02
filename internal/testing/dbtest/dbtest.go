// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dbtest provides a parallel-safe test database harness. Each
// caller gets a freshly cloned database from a template that has all
// migrations applied.
//
// Usage:
//
//	func TestSomething(t *testing.T) {
//	    pool := dbtest.NewTestDB(t)
//	    // pool is a *pgxpool.Pool against a freshly cloned DB.
//	}
//
// The harness reads SHITHUB_TEST_DATABASE_URL for the bootstrap connection
// (used to CREATE/DROP databases). Tests are skipped if the env var is not
// set, so unit tests stay green on machines without Postgres.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/db"
	_ "github.com/tenseleyFlow/shithub/internal/migrationsfs" // register migrations
)

const envURL = "SHITHUB_TEST_DATABASE_URL"

var (
	templateOnce  sync.Once
	templateName  string
	templateError error
)

// NewTestDB returns a *pgxpool.Pool against a freshly created database
// cloned from the per-test-suite template. The database is dropped on
// t.Cleanup. Calls t.Skip if SHITHUB_TEST_DATABASE_URL is unset.
//
// Accepts testing.TB so benchmarks (*testing.B) can use the same
// fixture as unit tests (*testing.T) — PRO-EXT_SR-08 added the first
// benchmark consumer.
func NewTestDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	bootURL := os.Getenv(envURL)
	if bootURL == "" {
		t.Skipf("dbtest: %s not set; skipping integration test", envURL)
	}

	templateOnce.Do(func() {
		templateName, templateError = ensureTemplate(bootURL)
	})
	if templateError != nil {
		t.Fatalf("dbtest: template setup: %v", templateError)
	}

	dbName := uniqueDBName()
	if err := createFromTemplate(bootURL, dbName, templateName); err != nil {
		t.Fatalf("dbtest: clone db: %v", err)
	}
	t.Cleanup(func() {
		if err := dropDB(bootURL, dbName); err != nil {
			t.Logf("dbtest: drop %s: %v", dbName, err)
		}
	})

	pool, err := db.Open(context.Background(), db.Config{
		URL:      replaceDBName(bootURL, dbName),
		MaxConns: 4,
		MinConns: 0,
	})
	if err != nil {
		t.Fatalf("dbtest: open clone: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ensureTemplate creates the template database (if absent) and applies all
// migrations to it. Idempotent: subsequent runs reuse the existing template.
// templateLockKey is the pg_advisory_lock key that serializes template
// setup across test *processes*. templateOnce only covers one process,
// but `go test ./...` runs packages concurrently against the same
// Postgres, and two packages migrating the template at once collide
// on CREATE TYPE / CREATE INDEX (SQLSTATE 23505 / 42P07).
const templateLockKey int64 = 7413200902

func ensureTemplate(bootURL string) (string, error) {
	const name = "shithub_test_template"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Hold a session-level advisory lock on the boot connection for the
	// whole create+migrate sequence. The lock lives on this connection,
	// so it must stay open until we are done; migrations use their own
	// connection to the template database.
	lockConn, err := pgx.Connect(ctx, bootURL)
	if err != nil {
		return "", fmt.Errorf("dbtest: connect: %w", err)
	}
	defer func() { _ = lockConn.Close(context.Background()) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", templateLockKey); err != nil {
		return "", fmt.Errorf("dbtest: template lock: %w", err)
	}
	defer func() { _, _ = lockConn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", templateLockKey) }()

	exists, err := dbExists(ctx, bootURL, name)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := createDB(bootURL, name); err != nil {
			return "", err
		}
	}
	tplURL := replaceDBName(bootURL, name)
	if err := db.Migrate(ctx, db.Config{URL: tplURL}, db.MigrateUp); err != nil {
		return "", fmt.Errorf("dbtest: migrate template: %w", err)
	}
	// Marking IS_TEMPLATE lets non-superusers clone it; ignore failures
	// (e.g. insufficient privilege) because cloning by the owner works
	// regardless.
	if err := execBoot(bootURL, "ALTER DATABASE "+quoteIdent(name)+" IS_TEMPLATE TRUE"); err != nil {
		_ = err
	}
	return name, nil
}

func dbExists(ctx context.Context, bootURL, name string) (bool, error) {
	conn, err := pgx.Connect(ctx, bootURL)
	if err != nil {
		return false, fmt.Errorf("dbtest: connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var exists bool
	err = conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("dbtest: pg_database query: %w", err)
	}
	return exists, nil
}

func createDB(bootURL, name string) error {
	return execBoot(bootURL, "CREATE DATABASE "+quoteIdent(name))
}

func dropDB(bootURL, name string) error {
	// Force-disconnect any leftover sessions; harmless if there are none.
	_ = execBoot(bootURL, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = "+quoteLiteral(name))
	return execBoot(bootURL, "DROP DATABASE IF EXISTS "+quoteIdent(name))
}

func createFromTemplate(bootURL, name, template string) error {
	return execBoot(bootURL, "CREATE DATABASE "+quoteIdent(name)+" TEMPLATE "+quoteIdent(template))
}

func execBoot(bootURL, sql string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, bootURL)
	if err != nil {
		return fmt.Errorf("dbtest: connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, sql)
	if err != nil {
		return fmt.Errorf("dbtest: %s: %w", sql, err)
	}
	return nil
}

// replaceDBName rewrites the path component of a postgres URL to point at a
// different database.
func replaceDBName(bootURL, name string) string {
	u, err := url.Parse(bootURL)
	if err != nil {
		return bootURL
	}
	u.Path = "/" + name
	return u.String()
}

// uniqueDBName returns a per-test-database name. Hex prefix avoids clashes
// across parallel test runs.
func uniqueDBName() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "shithub_test_" + hex.EncodeToString(b[:])
}

// quoteIdent wraps an identifier in double quotes and escapes any embedded
// double quotes. For test-DB names we generate the names ourselves so this
// is purely belt-and-braces.
func quoteIdent(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, s[i])
	}
	out = append(out, '"')
	return string(out)
}

func quoteLiteral(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	out = append(out, '\'')
	return string(out)
}
