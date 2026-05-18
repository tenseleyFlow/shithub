// SPDX-License-Identifier: AGPL-3.0-or-later

// Package checksync mirrors Actions workflow job state into check_run rows.
package checksync

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
	"github.com/tenseleyFlow/shithub/internal/pulls/mergeenqueue"
)

// Deps wires check synchronization to postgres and logging.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// ChangedJobs best-effort syncs every job state that has a visible check_run
// status representation. Missing check rows are intentionally non-fatal.
func ChangedJobs(ctx context.Context, deps Deps, jobs []actionsdb.WorkflowJob) {
	for _, job := range jobs {
		if job.Status != actionsdb.WorkflowJobStatusRunning &&
			job.Status != actionsdb.WorkflowJobStatusCompleted &&
			job.Status != actionsdb.WorkflowJobStatusCancelled {
			continue
		}
		if err := Job(ctx, deps, job); err != nil && deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "actions checksync: sync check_run", "job_id", job.ID, "error", err)
		}
	}
}

// Job mirrors one Actions workflow_job row into its check_run row. Missing check
// rows are non-fatal because check creation is already best-effort in the
// trigger path and can be reconciled independently.
func Job(ctx context.Context, deps Deps, job actionsdb.WorkflowJob) error {
	if deps.Pool == nil {
		return errors.New("actions checksync: nil Pool")
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
	if err != nil {
		return err
	}
	// S64: when the Actions runner reports a terminal job state, fan out
	// pr:mergeability ticks to every open PR sharing the head SHA so a
	// required-checks gate can flip blocked → clean.
	if params.Status == "completed" {
		mergeenqueue.ForHeadSHA(ctx, deps.Pool, deps.Logger, run.RepoID, run.HeadSha)
	}
	return nil
}

func timeFromPg(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}
