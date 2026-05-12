// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cleanup owns background retention for Actions data that should not
// live forever: hot log chunks, expired artifact metadata/blob objects,
// terminal workflow runs, and consumed runner JWT audit rows.
package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// KindWorkflowCleanup names the worker job that applies Actions retention.
const KindWorkflowCleanup worker.Kind = "workflow:cleanup"

const (
	defaultStepLogChunkDays = 7
	defaultRunDays          = 365
	defaultJWTUsedDays      = 30
	defaultArtifactBatch    = 1000
	maxArtifactBatch        = 10000
	actionsRunsPrefix       = "actions/runs/"
)

// Payload is the workflow:cleanup worker payload. Zero values select the
// production defaults documented in docs/internal/actions-schema.md.
type Payload struct {
	StepLogChunkDays int `json:"step_log_chunk_days,omitempty"`
	RunDays          int `json:"run_days,omitempty"`
	JWTUsedDays      int `json:"jwt_used_days,omitempty"`
	ArtifactBatch    int `json:"artifact_batch,omitempty"`
}

// Deps are the runtime dependencies for Handler.
type Deps struct {
	Pool        *pgxpool.Pool
	ObjectStore storage.ObjectStore
	Logger      *slog.Logger
	Now         func() time.Time
}

// Result summarizes one cleanup sweep.
type Result struct {
	ChunksDeleted          int64
	ArtifactRowsDeleted    int64
	ArtifactObjectsDeleted int64
	RunsDeleted            int64
	JWTUsedDeleted         int64
}

// Handler returns the worker handler for workflow:cleanup.
func Handler(deps Deps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p Payload
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return worker.PoisonError(fmt.Errorf("workflow cleanup: bad payload: %w", err))
			}
		}
		res, err := Sweep(ctx, deps, p)
		if err != nil {
			return err
		}
		if deps.Logger != nil {
			deps.Logger.InfoContext(ctx, "workflow cleanup complete",
				"chunks_deleted", res.ChunksDeleted,
				"artifact_rows_deleted", res.ArtifactRowsDeleted,
				"artifact_objects_deleted", res.ArtifactObjectsDeleted,
				"runs_deleted", res.RunsDeleted,
				"jwt_used_deleted", res.JWTUsedDeleted)
		}
		return nil
	}
}

// Sweep applies Actions retention once.
func Sweep(ctx context.Context, deps Deps, p Payload) (Result, error) {
	if deps.Pool == nil {
		return Result{}, worker.PoisonError(errors.New("workflow cleanup: database pool is not configured"))
	}
	if err := normalizePayload(&p); err != nil {
		return Result{}, worker.PoisonError(err)
	}
	now := time.Now().UTC()
	if deps.Now != nil {
		now = deps.Now().UTC()
	}
	q := actionsdb.New()
	res := Result{}

	stepCutoff := pgtype.Timestamptz{Time: now.Add(-time.Duration(p.StepLogChunkDays) * 24 * time.Hour), Valid: true}
	n, err := q.DeleteStaleStepLogChunksForCleanup(ctx, deps.Pool, stepCutoff)
	if err != nil {
		return Result{}, fmt.Errorf("workflow cleanup: delete stale log chunks: %w", err)
	}
	res.ChunksDeleted = n
	recordPruned("chunks", n)

	artifactCutoff := pgtype.Timestamptz{Time: now, Valid: true}
	artifactRes, err := pruneExpiredArtifacts(ctx, q, deps, artifactCutoff, int32(p.ArtifactBatch))
	if err != nil {
		return Result{}, err
	}
	res.ArtifactRowsDeleted = artifactRes.ArtifactRowsDeleted
	res.ArtifactObjectsDeleted = artifactRes.ArtifactObjectsDeleted
	recordPruned("blobs", artifactRes.ArtifactObjectsDeleted)

	runCutoff := pgtype.Timestamptz{Time: now.Add(-time.Duration(p.RunDays) * 24 * time.Hour), Valid: true}
	n, err = q.DeleteOldWorkflowRunsForCleanup(ctx, deps.Pool, runCutoff)
	if err != nil {
		return Result{}, fmt.Errorf("workflow cleanup: delete old workflow runs: %w", err)
	}
	res.RunsDeleted = n
	recordPruned("runs", n)

	jwtCutoff := pgtype.Timestamptz{Time: now.Add(-time.Duration(p.JWTUsedDays) * 24 * time.Hour), Valid: true}
	n, err = q.DeleteOldRunnerJWTUsesForCleanup(ctx, deps.Pool, jwtCutoff)
	if err != nil {
		return Result{}, fmt.Errorf("workflow cleanup: delete old runner JWT uses: %w", err)
	}
	res.JWTUsedDeleted = n
	recordPruned("jwt_used", n)

	return res, nil
}

func normalizePayload(p *Payload) error {
	if p.StepLogChunkDays < 0 || p.RunDays < 0 || p.JWTUsedDays < 0 || p.ArtifactBatch < 0 {
		return errors.New("workflow cleanup: retention values must be non-negative")
	}
	if p.StepLogChunkDays == 0 {
		p.StepLogChunkDays = defaultStepLogChunkDays
	}
	if p.RunDays == 0 {
		p.RunDays = defaultRunDays
	}
	if p.JWTUsedDays == 0 {
		p.JWTUsedDays = defaultJWTUsedDays
	}
	if p.ArtifactBatch == 0 {
		p.ArtifactBatch = defaultArtifactBatch
	}
	if p.ArtifactBatch > maxArtifactBatch {
		return fmt.Errorf("workflow cleanup: artifact_batch must be <= %d", maxArtifactBatch)
	}
	return nil
}

func pruneExpiredArtifacts(
	ctx context.Context,
	q *actionsdb.Queries,
	deps Deps,
	cutoff pgtype.Timestamptz,
	batch int32,
) (Result, error) {
	if deps.ObjectStore == nil {
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "workflow cleanup: object storage not configured; skipping artifact pruning")
		}
		return Result{}, nil
	}

	var res Result
	for {
		rows, err := q.ListExpiredWorkflowArtifactsForCleanup(ctx, deps.Pool, actionsdb.ListExpiredWorkflowArtifactsForCleanupParams{
			ExpiresAt: cutoff,
			Limit:     batch,
		})
		if err != nil {
			return Result{}, fmt.Errorf("workflow cleanup: list expired artifacts: %w", err)
		}
		if len(rows) == 0 {
			return res, nil
		}

		ids := make([]int64, 0, len(rows))
		for _, row := range rows {
			if !strings.HasPrefix(row.ObjectKey, actionsRunsPrefix) {
				return Result{}, fmt.Errorf("workflow cleanup: refusing to delete non-actions object key %q", row.ObjectKey)
			}
			if err := deps.ObjectStore.Delete(ctx, row.ObjectKey); err != nil {
				return Result{}, fmt.Errorf("workflow cleanup: delete artifact object %q: %w", row.ObjectKey, err)
			}
			res.ArtifactObjectsDeleted++
			ids = append(ids, row.ID)
		}

		deleted, err := q.DeleteWorkflowArtifactsByIDs(ctx, deps.Pool, ids)
		if err != nil {
			return Result{}, fmt.Errorf("workflow cleanup: delete artifact rows: %w", err)
		}
		res.ArtifactRowsDeleted += deleted
		if len(rows) < int(batch) {
			return res, nil
		}
	}
}

func recordPruned(kind string, n int64) {
	if n <= 0 {
		return
	}
	metrics.ActionsRunsPrunedTotal.WithLabelValues(kind).Add(float64(n))
}
