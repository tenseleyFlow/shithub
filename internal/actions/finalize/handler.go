// SPDX-License-Identifier: AGPL-3.0-or-later

// Package finalize owns server-side Actions finalization work that should not
// run on the hot runner API path.
package finalize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// KindWorkflowFinalizeStep names the worker job that compacts per-step log
// chunks into object storage and prunes the SQL chunks.
const KindWorkflowFinalizeStep worker.Kind = "workflow:finalize_step"

const (
	defaultMaxLogBytes = 100 * 1024 * 1024
	chunkPageSize      = 1000
	logContentType     = "text/plain; charset=utf-8"
)

// Payload is the workflow:finalize_step worker payload.
type Payload struct {
	StepID int64 `json:"step_id"`
}

// Deps are the runtime dependencies for Handler.
type Deps struct {
	Pool        *pgxpool.Pool
	ObjectStore storage.ObjectStore
	Logger      *slog.Logger
	MaxLogBytes int64
}

// Handler returns the worker handler for workflow:finalize_step.
func Handler(deps Deps) worker.Handler {
	maxLogBytes := deps.MaxLogBytes
	if maxLogBytes <= 0 {
		maxLogBytes = defaultMaxLogBytes
	}
	return func(ctx context.Context, raw json.RawMessage) error {
		var p Payload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("finalize step: bad payload: %w", err))
		}
		if p.StepID <= 0 {
			return worker.PoisonError(errors.New("finalize step: missing step_id"))
		}
		if deps.Pool == nil {
			return worker.PoisonError(errors.New("finalize step: database pool is not configured"))
		}
		if deps.ObjectStore == nil {
			return worker.PoisonError(errors.New("finalize step: object storage is not configured"))
		}
		if err := finalizeStep(ctx, deps, p.StepID, maxLogBytes); err != nil {
			return err
		}
		return nil
	}
}

func finalizeStep(ctx context.Context, deps Deps, stepID, maxLogBytes int64) error {
	q := actionsdb.New()
	step, err := q.GetWorkflowStepByID(ctx, deps.Pool, stepID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return worker.PoisonError(fmt.Errorf("finalize step: step %d not found", stepID))
		}
		return fmt.Errorf("finalize step: load step: %w", err)
	}
	job, err := q.GetWorkflowJobByID(ctx, deps.Pool, step.JobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return worker.PoisonError(fmt.Errorf("finalize step: job %d not found", step.JobID))
		}
		return fmt.Errorf("finalize step: load job: %w", err)
	}

	log, err := collectChunks(ctx, q, deps.Pool, stepID, maxLogBytes)
	if err != nil {
		return err
	}
	if len(log) == 0 && step.LogObjectKey.Valid {
		return nil
	}

	objectKey := pgtype.Text{}
	if len(log) > 0 {
		key := StepLogObjectKey(job.RunID, job.ID, stepID)
		if _, err := deps.ObjectStore.Put(ctx, key, bytes.NewReader(log), storage.PutOpts{
			ContentType:   logContentType,
			ContentLength: int64(len(log)),
		}); err != nil {
			return fmt.Errorf("finalize step: upload log object: %w", err)
		}
		objectKey = pgtype.Text{String: key, Valid: true}
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("finalize step: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := q.UpdateWorkflowStepLogObject(ctx, tx, actionsdb.UpdateWorkflowStepLogObjectParams{
		ID:           stepID,
		LogObjectKey: objectKey,
		LogByteCount: int64(len(log)),
	}); err != nil {
		return fmt.Errorf("finalize step: update step log object: %w", err)
	}
	if err := q.DeleteStepLogChunks(ctx, tx, stepID); err != nil {
		return fmt.Errorf("finalize step: delete chunks: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("finalize step: commit: %w", err)
	}
	committed = true
	if deps.Logger != nil {
		deps.Logger.InfoContext(ctx, "finalized workflow step log",
			"step_id", stepID, "job_id", job.ID, "run_id", job.RunID, "bytes", len(log))
	}
	return nil
}

func collectChunks(ctx context.Context, q *actionsdb.Queries, db actionsdb.DBTX, stepID, maxLogBytes int64) ([]byte, error) {
	var (
		out     bytes.Buffer
		lastSeq int32 = -1
	)
	for {
		chunks, err := q.ListStepLogChunks(ctx, db, actionsdb.ListStepLogChunksParams{
			StepID: stepID,
			Seq:    lastSeq,
			Limit:  chunkPageSize,
		})
		if err != nil {
			return nil, fmt.Errorf("finalize step: list chunks: %w", err)
		}
		if len(chunks) == 0 {
			return out.Bytes(), nil
		}
		for _, chunk := range chunks {
			if int64(out.Len()+len(chunk.Chunk)) > maxLogBytes {
				return nil, worker.PoisonError(fmt.Errorf("finalize step: log exceeds %d bytes", maxLogBytes))
			}
			if _, err := out.Write(chunk.Chunk); err != nil {
				return nil, fmt.Errorf("finalize step: append chunk: %w", err)
			}
			lastSeq = chunk.Seq
		}
	}
}

// StepLogObjectKey returns the deterministic object key for a finalized step
// log. It is intentionally independent of step names so renames do not move
// already-uploaded logs.
func StepLogObjectKey(runID, jobID, stepID int64) string {
	return fmt.Sprintf("actions/runs/%d/jobs/%d/steps/%d.log", runID, jobID, stepID)
}
