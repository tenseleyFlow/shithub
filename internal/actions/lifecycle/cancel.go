// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lifecycle owns user-visible Actions run/job lifecycle mutations:
// cancellation, re-runs, and retention as the S41g slices land.
package lifecycle

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/checksync"
	actionsevents "github.com/tenseleyFlow/shithub/internal/actions/events"
	"github.com/tenseleyFlow/shithub/internal/actions/runstate"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
)

const (
	CancelReasonUser        = "user"
	CancelReasonConcurrency = "concurrency"
	CancelReasonTimeout     = "timeout"
)

// CancelResult summarizes the durable state changes from a cancel request.
type CancelResult struct {
	RunID         int64
	ChangedJobs   []actionsdb.WorkflowJob
	RunCompleted  bool
	RunConclusion actionsdb.CheckConclusion
}

// CancelRun requests cancellation for every queued/running job in a workflow
// run. Queued jobs become terminal immediately; running jobs keep running with
// cancel_requested=true so the runner's cancel-check loop can kill them.
func CancelRun(ctx context.Context, deps Deps, runID int64, reason string) (CancelResult, error) {
	if deps.Pool == nil {
		return CancelResult{}, errors.New("actions lifecycle: nil Pool")
	}
	q := actionsdb.New()
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return CancelResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	run, err := q.GetWorkflowRunByID(ctx, tx, runID)
	if err != nil {
		return CancelResult{}, err
	}
	changed, err := q.RequestWorkflowRunCancel(ctx, tx, runID)
	if err != nil {
		return CancelResult{}, err
	}
	for _, job := range changed {
		if job.Status == actionsdb.WorkflowJobStatusCancelled {
			if _, err := q.CancelOpenWorkflowStepsForJob(ctx, tx, job.ID); err != nil {
				return CancelResult{}, err
			}
		}
	}
	var (
		runCompleted  bool
		runConclusion actionsdb.CheckConclusion
	)
	if len(changed) > 0 {
		runCompleted, runConclusion, err = runstate.RollupAfterCancel(ctx, q, tx, runID)
		if err != nil {
			return CancelResult{}, err
		}
		run, err = q.GetWorkflowRunByID(ctx, tx, runID)
		if err != nil {
			return CancelResult{}, err
		}
		if err := emitCancelEvents(ctx, tx, run, changed, runCompleted); err != nil {
			return CancelResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CancelResult{}, err
	}
	committed = true

	recordCancelledJobs(changed, reason)
	syncChangedJobChecks(ctx, deps, changed)
	return CancelResult{
		RunID:         runID,
		ChangedJobs:   changed,
		RunCompleted:  runCompleted,
		RunConclusion: runConclusion,
	}, nil
}

// CancelJob requests cancellation for one queued/running job. Terminal jobs are
// a successful no-op so a cancel/complete race does not surface as an error.
func CancelJob(ctx context.Context, deps Deps, jobID int64, reason string) (CancelResult, error) {
	if deps.Pool == nil {
		return CancelResult{}, errors.New("actions lifecycle: nil Pool")
	}
	q := actionsdb.New()
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return CancelResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	changedJob, err := q.RequestWorkflowJobCancel(ctx, tx, jobID)
	var changed []actionsdb.WorkflowJob
	var runID int64
	switch {
	case err == nil:
		changed = []actionsdb.WorkflowJob{changedJob}
		runID = changedJob.RunID
		if changedJob.Status == actionsdb.WorkflowJobStatusCancelled {
			if _, err := q.CancelOpenWorkflowStepsForJob(ctx, tx, changedJob.ID); err != nil {
				return CancelResult{}, err
			}
		}
	case errors.Is(err, pgx.ErrNoRows):
		existing, getErr := q.GetWorkflowJobByID(ctx, tx, jobID)
		if getErr != nil {
			return CancelResult{}, getErr
		}
		runID = existing.RunID
	default:
		return CancelResult{}, err
	}

	var (
		runCompleted  bool
		runConclusion actionsdb.CheckConclusion
	)
	if len(changed) > 0 {
		runCompleted, runConclusion, err = runstate.RollupAfterCancel(ctx, q, tx, runID)
		if err != nil {
			return CancelResult{}, err
		}
		run, err := q.GetWorkflowRunByID(ctx, tx, runID)
		if err != nil {
			return CancelResult{}, err
		}
		if err := emitCancelEvents(ctx, tx, run, changed, runCompleted); err != nil {
			return CancelResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CancelResult{}, err
	}
	committed = true

	recordCancelledJobs(changed, reason)
	syncChangedJobChecks(ctx, deps, changed)
	return CancelResult{
		RunID:         runID,
		ChangedJobs:   changed,
		RunCompleted:  runCompleted,
		RunConclusion: runConclusion,
	}, nil
}

func recordCancelledJobs(jobs []actionsdb.WorkflowJob, reason string) {
	if len(jobs) == 0 {
		return
	}
	metrics.ActionsJobsCancelledTotal.WithLabelValues(cancelReason(reason)).Add(float64(len(jobs)))
}

func emitCancelEvents(ctx context.Context, tx pgx.Tx, run actionsdb.WorkflowRun, jobs []actionsdb.WorkflowJob, runCompleted bool) error {
	for _, job := range jobs {
		if job.Status != actionsdb.WorkflowJobStatusCancelled {
			continue
		}
		if err := actionsevents.EmitJobTx(ctx, tx, run, job, actionsevents.ActionCancelled); err != nil {
			return err
		}
	}
	if runCompleted {
		action := actionsevents.ActionCompleted
		if run.Status == actionsdb.WorkflowRunStatusCancelled {
			action = actionsevents.ActionCancelled
		}
		if err := actionsevents.EmitRunTx(ctx, tx, run, action); err != nil {
			return err
		}
	}
	return nil
}

func cancelReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case CancelReasonUser:
		return CancelReasonUser
	case CancelReasonConcurrency:
		return CancelReasonConcurrency
	case CancelReasonTimeout:
		return CancelReasonTimeout
	default:
		return CancelReasonUser
	}
}

func syncChangedJobChecks(ctx context.Context, deps Deps, jobs []actionsdb.WorkflowJob) {
	checksync.ChangedJobs(ctx, checksync.Deps{Pool: deps.Pool, Logger: deps.Logger}, jobs)
}

// SyncCheckRunForJob mirrors an Actions workflow_job row into its check_run
// row. Missing check rows are non-fatal because check creation is already
// best-effort in the trigger path and can be reconciled independently.
func SyncCheckRunForJob(ctx context.Context, deps Deps, job actionsdb.WorkflowJob) error {
	return checksync.Job(ctx, checksync.Deps{Pool: deps.Pool, Logger: deps.Logger}, job)
}
