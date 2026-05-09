// SPDX-License-Identifier: AGPL-3.0-or-later

// Package db owns the Postgres connection lifecycle. S01 ships the
// open/healthcheck/transaction helpers; later sprints add domain-specific
// wrappers but always go through this package for the pool.
package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config carries the pool's connection settings. S03 will populate this from
// the layered config loader; for S01 we accept an optional explicit URL or
// fall back to SHITHUB_DATABASE_URL.
type Config struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	ConnectTimeout  time.Duration
	StatementCancel time.Duration
}

// Defaults returns sensible defaults for a dev pool. Prod values land via
// the config loader in S03.
func Defaults() Config {
	return Config{
		MaxConns:        10,
		MinConns:        1,
		ConnectTimeout:  5 * time.Second,
		StatementCancel: 30 * time.Second,
	}
}

// Resolve fills in URL from env if missing and clamps numeric defaults.
func (c Config) Resolve() Config {
	if c.URL == "" {
		c.URL = os.Getenv("SHITHUB_DATABASE_URL")
	}
	if c.MaxConns <= 0 {
		c.MaxConns = 10
	}
	if c.MinConns < 0 {
		c.MinConns = 0
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 5 * time.Second
	}
	if c.StatementCancel <= 0 {
		c.StatementCancel = 30 * time.Second
	}
	return c
}

// ErrNoURL is returned by Open when neither cfg.URL nor SHITHUB_DATABASE_URL
// is set.
var ErrNoURL = errors.New("db: no DATABASE_URL configured (set SHITHUB_DATABASE_URL)")

// Open creates a new pgx pool from cfg. The caller owns the pool's lifecycle
// and must call pool.Close() on shutdown.
func Open(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	cfg = cfg.Resolve()
	if cfg.URL == "" {
		return nil, ErrNoURL
	}

	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}
	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	// QueryCounter is a no-op when the request context wasn't built
	// with WithCounter — production traffic pays one map lookup per
	// query. Tests that assert "this route does ≤ N queries" wrap the
	// request context to opt in.
	pcfg.ConnConfig.Tracer = QueryCounter{}

	openCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(openCtx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}

	// Verify connectivity before returning. A pool that can't talk to
	// Postgres at startup is a hard failure, not a retry-on-first-query
	// situation.
	if err := pool.Ping(openCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return pool, nil
}

// Healthcheck performs a fast SELECT 1 against the pool with a short
// timeout. Used by /readyz.
func Healthcheck(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("db: nil pool")
	}
	hc, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var v int
	if err := pool.QueryRow(hc, "SELECT 1").Scan(&v); err != nil {
		return fmt.Errorf("db: healthcheck: %w", err)
	}
	if v != 1 {
		return fmt.Errorf("db: healthcheck: unexpected scalar %d", v)
	}
	return nil
}

// WithTx runs fn inside a Postgres transaction, committing on nil error and
// rolling back otherwise. Panics inside fn are recovered and re-raised after
// rollback so callers see them as panics rather than silent commits.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = fmt.Errorf("%w (rollback: %v)", err, rbErr)
			}
			return
		}
		if cmErr := tx.Commit(ctx); cmErr != nil {
			err = fmt.Errorf("db: commit: %w", cmErr)
		}
	}()
	err = fn(tx)
	return err
}
