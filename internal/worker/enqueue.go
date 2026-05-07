// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"
)

// DBTX matches the pgx interface that sqlc-generated methods accept
// (anything providing Exec/Query/QueryRow). The pool, a tx, and the
// helpers in dbtest all satisfy it.
type DBTX = workerdb.DBTX

// EnqueueOptions tunes a single enqueue. RunAt zero means "now"; pass a
// future time to delay the first run. MaxAttempts zero means use the
// table default (5).
type EnqueueOptions struct {
	RunAt       pgtype.Timestamptz
	MaxAttempts int32
}

// Enqueue inserts a job row and returns its id. Callers running inside a
// transaction should pass the tx as db so the enqueue is rolled back
// alongside any related state changes; same goes for the NOTIFY (issued
// separately by the caller via Notify).
func Enqueue(ctx context.Context, db DBTX, kind Kind, payload any, opts EnqueueOptions) (int64, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("worker: marshal payload: %w", err)
	}
	q := workerdb.New()
	row, err := q.EnqueueJob(ctx, db, workerdb.EnqueueJobParams{
		Kind:        string(kind),
		Payload:     body,
		RunAt:       opts.RunAt,
		MaxAttempts: pgtype.Int4{Int32: opts.MaxAttempts, Valid: opts.MaxAttempts > 0},
	})
	if err != nil {
		return 0, fmt.Errorf("worker: enqueue %s: %w", kind, err)
	}
	return row.ID, nil
}

// Notify wakes any LISTENing workers. Safe to call after a successful
// commit; if called inside a tx, the NOTIFY only delivers when the tx
// commits (Postgres semantics). Errors are non-fatal — workers also poll
// at a slow interval as a backstop.
func Notify(ctx context.Context, db DBTX) error {
	_, err := db.Exec(ctx, "SELECT pg_notify($1, '')", NotifyChannel)
	return err
}
