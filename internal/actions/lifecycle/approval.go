// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/actions/checksync"
	actionsevents "github.com/tenseleyFlow/shithub/internal/actions/events"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
)

var (
	ErrApprovalActorRequired = errors.New("actions lifecycle: approval actor required")
	ErrRunNotApprovalPending = errors.New("actions lifecycle: run is not pending approval")
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

func workflowRunFromApprovalRow(row actionsdb.ApproveWorkflowRunRow) actionsdb.WorkflowRun {
	return actionsdb.WorkflowRun{
		ID:               row.ID,
		RepoID:           row.RepoID,
		RunIndex:         row.RunIndex,
		WorkflowFile:     row.WorkflowFile,
		WorkflowName:     row.WorkflowName,
		HeadSha:          row.HeadSha,
		HeadRef:          row.HeadRef,
		Event:            row.Event,
		EventPayload:     row.EventPayload,
		ActorUserID:      row.ActorUserID,
		ParentRunID:      row.ParentRunID,
		ConcurrencyGroup: row.ConcurrencyGroup,
		Status:           row.Status,
		Conclusion:       row.Conclusion,
		Pinned:           row.Pinned,
		NeedApproval:     row.NeedApproval,
		ApprovedByUserID: row.ApprovedByUserID,
		StartedAt:        row.StartedAt,
		CompletedAt:      row.CompletedAt,
		Version:          row.Version,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
		TriggerEventID:   row.TriggerEventID,
	}
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
