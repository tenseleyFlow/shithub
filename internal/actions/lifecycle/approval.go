// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/actions/checksync"
	actionsevents "github.com/tenseleyFlow/shithub/internal/actions/events"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
)

var (
	ErrApprovalActorRequired     = errors.New("actions lifecycle: approval actor required")
	ErrRunNotApprovalPending     = errors.New("actions lifecycle: run is not pending approval")
	ErrApprovalSelfReviewBlocked = errors.New("actions lifecycle: approval actor cannot self-review this environment")
)

type ApprovalResult struct {
	Run         actionsdb.WorkflowRun
	ChangedJobs []actionsdb.WorkflowJob
}

// ApproveRun records a maintainer approval. The existing queued jobs remain
// queued; the runner claim query re-evaluates dispatch naturally, so approval
// never duplicates workflow_runs or workflow_jobs.
func ApproveRun(ctx context.Context, deps Deps, runID, actorUserID int64) (ApprovalResult, error) {
	if actorUserID == 0 {
		return ApprovalResult{}, ErrApprovalActorRequired
	}
	q := actionsdb.New()
	pending, err := q.GetWorkflowRunByID(ctx, deps.Pool, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApprovalResult{}, ErrRunNotApprovalPending
		}
		return ApprovalResult{}, err
	}
	if pending.Status == actionsdb.WorkflowRunStatusQueued &&
		pending.NeedApproval &&
		!pending.ApprovedByUserID.Valid &&
		pending.ActorUserID.Valid &&
		pending.ActorUserID.Int64 == actorUserID {
		blocked, err := environmentSelfReviewBlocked(ctx, q, deps.Pool, pending)
		if err != nil {
			return ApprovalResult{}, err
		}
		if blocked {
			return ApprovalResult{}, ErrApprovalSelfReviewBlocked
		}
	}
	run, err := q.ApproveWorkflowRun(ctx, deps.Pool, actionsdb.ApproveWorkflowRunParams{
		ID: runID,
		ApprovedByUserID: pgtype.Int8{
			Int64: actorUserID,
			Valid: true,
		},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApprovalResult{}, ErrRunNotApprovalPending
		}
		return ApprovalResult{}, err
	}
	return ApprovalResult{Run: workflowRunFromApprovalRow(run)}, nil
}

func environmentSelfReviewBlocked(ctx context.Context, q *actionsdb.Queries, db actionsdb.DBTX, run actionsdb.WorkflowRun) (bool, error) {
	jobs, err := q.ListJobsForRun(ctx, db, run.ID)
	if err != nil {
		return false, err
	}
	seen := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		name := strings.TrimSpace(job.EnvironmentName)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		env, err := q.GetRepoEnvironmentByName(ctx, db, actionsdb.GetRepoEnvironmentByNameParams{
			RepoID: run.RepoID,
			Name:   name,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return false, err
		}
		if env.RequiredReviewersEnabled && env.PreventSelfReview {
			return true, nil
		}
	}
	return false, nil
}

func workflowRunFromApprovalRow(row actionsdb.ApproveWorkflowRunRow) actionsdb.WorkflowRun {
	return actionsdb.WorkflowRun(row)
}

// RejectRun turns a pending-approval run into a completed/action_required run
// and mirrors every queued job to its check_run. It only acts on runs that are
// still pending approval, so an approve/reject race resolves cleanly.
func RejectRun(ctx context.Context, deps Deps, runID, actorUserID int64) (ApprovalResult, error) {
	if actorUserID == 0 {
		return ApprovalResult{}, ErrApprovalActorRequired
	}
	q := actionsdb.New()
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return ApprovalResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := q.RejectWorkflowRunApproval(ctx, tx, actionsdb.RejectWorkflowRunApprovalParams{
		RunID: runID,
		RejectedByUserID: pgtype.Int8{
			Int64: actorUserID,
			Valid: true,
		},
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApprovalResult{}, ErrRunNotApprovalPending
		}
		return ApprovalResult{}, err
	}
	jobs, err := q.MarkWorkflowJobsRejected(ctx, tx, runID)
	if err != nil {
		return ApprovalResult{}, err
	}
	run, err := q.MarkWorkflowRunRejected(ctx, tx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApprovalResult{}, ErrRunNotApprovalPending
		}
		return ApprovalResult{}, err
	}
	for _, job := range jobs {
		if err := actionsevents.EmitJobTx(ctx, tx, run, job, actionsevents.ActionCancelled); err != nil {
			return ApprovalResult{}, err
		}
	}
	if err := actionsevents.EmitRunTx(ctx, tx, run, actionsevents.ActionCompleted); err != nil {
		return ApprovalResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApprovalResult{}, err
	}
	committed = true
	checksync.ChangedJobs(ctx, checksync.Deps{Pool: deps.Pool, Logger: deps.Logger}, jobs)
	return ApprovalResult{Run: run, ChangedJobs: jobs}, nil
}
