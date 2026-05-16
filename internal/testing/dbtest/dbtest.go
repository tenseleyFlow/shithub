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
func ensureTemplate(bootURL string) (string, error) {
	const name = "shithub_test_template"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exists, err := dbExists(ctx, bootURL, name)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := createDB(bootURL, name); err != nil {
			return "", err
		}
	}

	// Apply migrations to the template.
	tplURL := replaceDBName(bootURL, name)
	if err := db.Migrate(ctx, db.Config{URL: tplURL}, db.MigrateUp); err != nil {
		return "", fmt.Errorf("dbtest: migrate template: %w", err)
	}

	// Mark the database as a template so CREATE ... TEMPLATE works without
	// requiring superuser. (PG allows a non-template database as TEMPLATE
	// when no other connections exist; marking it explicitly is cleaner.)
	if err := execBoot(bootURL, "ALTER DATABASE "+quoteIdent(name)+" IS_TEMPLATE TRUE"); err != nil {
		// Some Postgres versions/configs reject this; tolerate.
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
