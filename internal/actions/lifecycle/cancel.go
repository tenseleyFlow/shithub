// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lifecycle owns user-visible Actions run/job lifecycle mutations:
// cancellation now, with re-runs and retention following in later S41g slices.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/checks"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
)

const (
	CancelReasonUser        = "user"
	CancelReasonConcurrency = "concurrency"
	CancelReasonTimeout     = "timeout"
)

// Deps wires lifecycle operations to postgres and optional warning logs.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

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

	if _, err := q.GetWorkflowRunByID(ctx, tx, runID); err != nil {
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
		runCompleted, runConclusion, err = rollupRunAfterCancel(ctx, q, tx, runID)
		if err != nil {
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
		runCompleted, runConclusion, err = rollupRunAfterCancel(ctx, q, tx, runID)
		if err != nil {
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

func rollupRunAfterCancel(
	ctx context.Context,
	q *actionsdb.Queries,
	tx pgx.Tx,
	runID int64,
) (bool, actionsdb.CheckConclusion, error) {
	jobs, err := q.ListJobsForRun(ctx, tx, runID)
	if err != nil {
		return false, "", err
	}
	runConclusion, complete := deriveWorkflowRunConclusion(jobs)
	if complete {
		if _, err := q.CompleteWorkflowRun(ctx, tx, actionsdb.CompleteWorkflowRunParams{
			ID:         runID,
			Conclusion: runConclusion,
		}); err != nil {
			return false, "", err
		}
		return true, runConclusion, nil
	}
	if err := q.MarkWorkflowRunRunning(ctx, tx, runID); err != nil {
		return false, "", err
	}
	return false, "", nil
}

func deriveWorkflowRunConclusion(jobs []actionsdb.ListJobsForRunRow) (actionsdb.CheckConclusion, bool) {
	if len(jobs) == 0 {
		return actionsdb.CheckConclusionFailure, true
	}
	worst := actionsdb.CheckConclusionSuccess
	for _, job := range jobs {
		switch job.Status {
		case actionsdb.WorkflowJobStatusCompleted, actionsdb.WorkflowJobStatusCancelled, actionsdb.WorkflowJobStatusSkipped:
		default:
			return "", false
		}
		if job.Status == actionsdb.WorkflowJobStatusCancelled {
			worst = actionsdb.CheckConclusionCancelled
			continue
		}
		if !job.Conclusion.Valid {
			return actionsdb.CheckConclusionFailure, true
		}
		c := job.Conclusion.CheckConclusion
		if c == actionsdb.CheckConclusionFailure ||
			c == actionsdb.CheckConclusionTimedOut ||
			c == actionsdb.CheckConclusionActionRequired {
			return c, true
		}
		if c == actionsdb.CheckConclusionCancelled {
			worst = actionsdb.CheckConclusionCancelled
		}
	}
	return worst, true
}

func recordCancelledJobs(jobs []actionsdb.WorkflowJob, reason string) {
	if len(jobs) == 0 {
		return
	}
	metrics.ActionsJobsCancelledTotal.WithLabelValues(cancelReason(reason)).Add(float64(len(jobs)))
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
	for _, job := range jobs {
		if job.Status != actionsdb.WorkflowJobStatusRunning &&
			job.Status != actionsdb.WorkflowJobStatusCompleted &&
			job.Status != actionsdb.WorkflowJobStatusCancelled {
			continue
		}
		if err := SyncCheckRunForJob(ctx, deps, job); err != nil && deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "actions lifecycle: sync check_run", "job_id", job.ID, "error", err)
		}
	}
}

// SyncCheckRunForJob mirrors an Actions workflow_job row into its check_run
// row. Missing check rows are non-fatal because check creation is already
// best-effort in the trigger path and can be reconciled independently.
func SyncCheckRunForJob(ctx context.Context, deps Deps, job actionsdb.WorkflowJob) error {
	if deps.Pool == nil {
		return errors.New("actions lifecycle: nil Pool")
	}
	run, err := actionsdb.New().GetWorkflowRunByID(ctx, deps.Pool, job.RunID)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(job.JobName)
	if name == "" {
		name = job.JobKey
	}
	checkRun, err := checksdb.New().GetCheckRunByExternalID(ctx, deps.Pool, checksdb.GetCheckRunByExternalIDParams{
		RepoID:     run.RepoID,
		HeadSha:    run.HeadSha,
		Name:       name,
		ExternalID: pgtype.Text{String: fmt.Sprintf("workflow_run:%d:job:%s", job.RunID, job.JobKey), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	params := checks.UpdateParams{
		RunID:        checkRun.ID,
		HasStatus:    true,
		HasStartedAt: true,
		StartedAt:    timeFromPg(job.StartedAt),
	}
	switch job.Status {
	case actionsdb.WorkflowJobStatusRunning:
		params.Status = "in_progress"
	case actionsdb.WorkflowJobStatusCompleted, actionsdb.WorkflowJobStatusCancelled:
		params.Status = "completed"
		params.HasConclusion = true
		if job.Conclusion.Valid {
			params.Conclusion = string(job.Conclusion.CheckConclusion)
		} else if job.Status == actionsdb.WorkflowJobStatusCancelled {
			params.Conclusion = string(actionsdb.CheckConclusionCancelled)
		}
		params.HasCompletedAt = true
		params.CompletedAt = timeFromPg(job.CompletedAt)
	default:
		return nil
	}
	_, err = checks.Update(ctx, checks.Deps{Pool: deps.Pool, Logger: deps.Logger}, params)
	return err
}

func timeFromPg(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}
