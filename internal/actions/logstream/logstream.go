// SPDX-License-Identifier: AGPL-3.0-or-later

// Package logstream owns the small Postgres LISTEN/NOTIFY contract used by
// Actions step-log live tailing. NOTIFY payloads intentionally carry only the
// chunk sequence or terminal marker; the SSE handler reads chunk bytes from
// workflow_step_log_chunks so verbose logs never hit Postgres's payload cap.
package logstream

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const donePayload = "done"

// DBTX is the Exec-only subset shared by pgxpool.Pool and pgx.Tx.
type DBTX interface {
	Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error)
}

// Channel returns the per-step NOTIFY channel. stepID comes from postgres, so
// the numeric suffix is stable and safe to expose as a channel component.
func Channel(stepID int64) string {
	return "step_log_" + strconv.FormatInt(stepID, 10)
}

// ListenSQL returns a quoted LISTEN statement for the per-step channel.
func ListenSQL(stepID int64) string {
	return "LISTEN " + pgx.Identifier{Channel(stepID)}.Sanitize()
}

// NotifyChunk wakes log tailers for a newly-persisted chunk.
func NotifyChunk(ctx context.Context, db DBTX, stepID int64, seq int32) error {
	return notify(ctx, db, stepID, strconv.FormatInt(int64(seq), 10))
}

// NotifyDone wakes log tailers and tells them to send the final done event.
func NotifyDone(ctx context.Context, db DBTX, stepID int64) error {
	return notify(ctx, db, stepID, donePayload)
}

// ParsePayload parses the NOTIFY payload.
func ParsePayload(payload string) (seq int32, done bool, ok bool) {
	payload = strings.TrimSpace(payload)
	if payload == donePayload {
		return 0, true, true
	}
	n, err := strconv.ParseInt(payload, 10, 32)
	if err != nil || n < 0 {
		return 0, false, false
	}
	return int32(n), false, true
}

func notify(ctx context.Context, db DBTX, stepID int64, payload string) error {
	_, err := db.Exec(ctx, "SELECT pg_notify($1, $2)", Channel(stepID), payload)
	return err
}
