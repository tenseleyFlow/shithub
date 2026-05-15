// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pagecache carries the cross-process plumbing that lets a
// worker job tell the web process to drop cached page renders for
// a (repo_id, branch_oid) pair. F01 PR-4 uses it to invalidate the
// commits-list LRU on push completion.
//
// The package itself is deliberately ignorant of any specific
// cache implementation — it ships a Publisher (called from the
// worker side) and a Listener (called from the web side with a
// caller-supplied apply callback). The neutral home keeps the
// dependency arrow `worker → web/handlers/repo/httpcache` from
// existing.
//
// Why this is needed even though the cache key includes the OID:
// the OID-keyed cache entries become unreachable on push (new
// requests resolve the new head OID → new key), so the 60s TTL is
// the dominant lever. PR-4's invalidation is a memory-recovery
// optimization plus belt-and-suspenders defense against future
// changes that might weaken the OID-keying contract.
package pagecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Channel is the Postgres NOTIFY channel name. Hardcoded so
// publisher and listener never drift.
const Channel = "pagecache_invalidate"

// InvalidatePayload is the JSON shape both sides serialize. Keep
// it small and stable — adding fields forces a rolling deploy
// that runs old + new in parallel for at least one cycle.
type InvalidatePayload struct {
	RepoID    int64  `json:"repo_id"`
	BranchOID string `json:"branch_oid"`
}

// DBTX is the minimal Exec surface Publish needs. *pgxpool.Pool
// and pgx.Tx both satisfy it; matches the worker package's own
// DBTX alias so callers don't juggle multiple shapes.
type DBTX interface {
	Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error)
}

// Publish broadcasts an invalidation hint over Postgres NOTIFY.
// Safe to call after a successful commit; if invoked inside a tx,
// the NOTIFY only delivers when the tx commits (Postgres
// semantics). Errors are non-fatal — the listener's failure mode
// is "TTL eats the staleness" and callers should not block on
// this.
func Publish(ctx context.Context, db DBTX, repoID int64, branchOID string) error {
	if db == nil {
		return errors.New("pagecache: nil db")
	}
	if branchOID == "" {
		return errors.New("pagecache: empty branchOID")
	}
	payload, err := json.Marshal(InvalidatePayload{RepoID: repoID, BranchOID: branchOID})
	if err != nil {
		return fmt.Errorf("pagecache: marshal payload: %w", err)
	}
	if _, err := db.Exec(ctx, "SELECT pg_notify($1, $2)", Channel, string(payload)); err != nil {
		return fmt.Errorf("pagecache: pg_notify: %w", err)
	}
	return nil
}

// ApplyFunc is the callback the Listen loop invokes for each
// well-formed invalidation. The web side wires this to its
// PageCache.InvalidateBranch.
type ApplyFunc func(repoID int64, branchOID string)

// Listen subscribes to Channel and applies each NOTIFY via apply.
// Blocks until ctx is canceled; restarts on transient connection
// errors with a brief backoff so a Postgres failover doesn't
// permanently silence invalidations. Run as a background
// goroutine during web boot.
//
// Logger is nil-safe: when nil, restart errors are silently
// swallowed and the loop retries.
func Listen(ctx context.Context, pool *pgxpool.Pool, apply ApplyFunc, logger *slog.Logger) {
	if pool == nil || apply == nil {
		return
	}
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		err := listenOnce(ctx, pool, apply, logger)
		if err != nil && !errors.Is(err, context.Canceled) && logger != nil {
			logger.WarnContext(ctx, "pagecache: listen restart", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func listenOnce(ctx context.Context, pool *pgxpool.Pool, apply ApplyFunc, logger *slog.Logger) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var p InvalidatePayload
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			if logger != nil {
				logger.WarnContext(ctx, "pagecache: bad payload",
					"payload", n.Payload, "error", err)
			}
			continue
		}
		if p.BranchOID == "" {
			continue
		}
		apply(p.RepoID, p.BranchOID)
	}
}
