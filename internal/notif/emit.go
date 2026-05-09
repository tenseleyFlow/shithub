// SPDX-License-Identifier: AGPL-3.0-or-later

package notif

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// Event is the call-site shape for an emitter. The notif package
// owns the schema-side details (which columns are NULL, payload
// shape, JSON marshal); callers fill in the semantic fields.
//
// Mentions, when present, are surfaced to the fan-out worker via the
// `mentions` JSON key in payload — kept here so callers don't have
// to remember the wire convention.
type Event struct {
	ActorUserID int64  // 0 → system event (NULL in DB).
	Kind        string // e.g. "issue_comment_created", "pr_review_requested".
	RepoID      int64  // 0 → user-scoped event (NULL).
	SourceKind  string // "issue", "pull", "repo", "user", …
	SourceID    int64
	Public      bool    // matches repo visibility (caller decides).
	Mentions    []int64 // resolved user-ids to fan out @-mentions to.
	Extra       map[string]any
}

// Emit inserts one row into domain_events and wakes the worker pool.
// Use within a tx via EmitTx when the emit must atomically commit
// with the source change (the typical case — issue.Create + the
// matching event row land together or not at all).
//
// The emit side is intentionally thin: the routing matrix and
// recipient computation live entirely in the fan-out worker so
// callers don't grow a per-kind switch.
func Emit(ctx context.Context, pool *pgxpool.Pool, ev Event) error {
	if err := emitInto(ctx, pool, ev); err != nil {
		return err
	}
	// Wake the worker pool. Best-effort — the cron-driven schedule
	// catches up if NOTIFY is dropped (LISTEN reconnects on its own).
	_ = worker.Notify(ctx, pool)
	return nil
}

// EmitTx is the in-tx variant. Use it when the source mutation and
// the event row must commit together (almost always). The NOTIFY is
// deferred to commit by Postgres semantics; this matches the rest of
// the worker's enqueue patterns.
func EmitTx(ctx context.Context, tx pgx.Tx, ev Event) error {
	return emitInto(ctx, tx, ev)
}

func emitInto(ctx context.Context, db socialdb.DBTX, ev Event) error {
	payload, err := buildPayload(ev)
	if err != nil {
		return fmt.Errorf("notif emit: payload: %w", err)
	}
	if _, err := socialdb.New().InsertDomainEvent(ctx, db, socialdb.InsertDomainEventParams{
		ActorUserID: pgInt8(ev.ActorUserID),
		Kind:        ev.Kind,
		RepoID:      pgInt8(ev.RepoID),
		SourceKind:  ev.SourceKind,
		SourceID:    ev.SourceID,
		Public:      ev.Public,
		Payload:     payload,
	}); err != nil {
		return fmt.Errorf("notif emit: insert: %w", err)
	}
	return nil
}

func buildPayload(ev Event) ([]byte, error) {
	out := map[string]any{}
	for k, v := range ev.Extra {
		out[k] = v
	}
	if len(ev.Mentions) > 0 {
		out["mentions"] = ev.Mentions
	}
	if len(out) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(out)
}

func pgInt8(v int64) pgtype.Int8 {
	return pgtype.Int8{Int64: v, Valid: v != 0}
}
