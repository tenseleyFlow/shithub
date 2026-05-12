// SPDX-License-Identifier: AGPL-3.0-or-later

// Package events emits webhook-facing Actions lifecycle events.
//
// These payloads are deliberately structural. Do not include workflow
// event_payload, env, permissions, step logs, runner JWTs, or secret material.
package events

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/notif"
)

const (
	KindWorkflowRun = "workflow_run"
	KindWorkflowJob = "workflow_job"

	ActionQueued    = "queued"
	ActionRunning   = "running"
	ActionCompleted = "completed"
	ActionCancelled = "cancelled"
)

// EmitRunTx writes one workflow_run domain event inside the caller's
// transaction. Use it when the run mutation and event row must commit
// atomically.
func EmitRunTx(ctx context.Context, tx pgx.Tx, run actionsdb.WorkflowRun, action string) error {
	return notif.EmitTx(ctx, tx, runEvent(run, action))
}

// EmitJobTx writes one workflow_job domain event inside the caller's
// transaction. The parent run snapshot is included so webhook subscribers do
// not need a second API call to identify the workflow execution.
func EmitJobTx(ctx context.Context, tx pgx.Tx, run actionsdb.WorkflowRun, job actionsdb.WorkflowJob, action string) error {
	return notif.EmitTx(ctx, tx, jobEvent(run, job, action))
}

func runEvent(run actionsdb.WorkflowRun, action string) notif.Event {
	return notif.Event{
		ActorUserID: int8Value(run.ActorUserID),
		Kind:        KindWorkflowRun,
		RepoID:      run.RepoID,
		SourceKind:  KindWorkflowRun,
		SourceID:    run.ID,
		Public:      false,
		Extra: map[string]any{
			"action":       action,
			"workflow_run": runPayload(run),
		},
	}
}

func jobEvent(run actionsdb.WorkflowRun, job actionsdb.WorkflowJob, action string) notif.Event {
	return notif.Event{
		ActorUserID: int8Value(run.ActorUserID),
		Kind:        KindWorkflowJob,
		RepoID:      run.RepoID,
		SourceKind:  KindWorkflowJob,
		SourceID:    job.ID,
		Public:      false,
		Extra: map[string]any{
			"action":       action,
			"workflow_run": runPayload(run),
			"workflow_job": jobPayload(job),
		},
	}
}

func runPayload(run actionsdb.WorkflowRun) map[string]any {
	return map[string]any{
		"id":               run.ID,
		"repo_id":          run.RepoID,
		"run_index":        run.RunIndex,
		"workflow_file":    run.WorkflowFile,
		"workflow_name":    run.WorkflowName,
		"head_sha":         run.HeadSha,
		"head_ref":         run.HeadRef,
		"event":            string(run.Event),
		"status":           string(run.Status),
		"conclusion":       conclusionValue(run.Conclusion),
		"actor_user_id":    int8Nullable(run.ActorUserID),
		"parent_run_id":    int8Nullable(run.ParentRunID),
		"created_at":       timeValue(run.CreatedAt),
		"updated_at":       timeValue(run.UpdatedAt),
		"started_at":       timeValue(run.StartedAt),
		"completed_at":     timeValue(run.CompletedAt),
		"trigger_event_id": run.TriggerEventID,
	}
}

func jobPayload(job actionsdb.WorkflowJob) map[string]any {
	return map[string]any{
		"id":               job.ID,
		"run_id":           job.RunID,
		"job_index":        job.JobIndex,
		"job_key":          job.JobKey,
		"job_name":         job.JobName,
		"runs_on":          job.RunsOn,
		"runner_id":        int8Nullable(job.RunnerID),
		"needs_jobs":       job.NeedsJobs,
		"timeout_minutes":  job.TimeoutMinutes,
		"status":           string(job.Status),
		"conclusion":       conclusionValue(job.Conclusion),
		"cancel_requested": job.CancelRequested,
		"created_at":       timeValue(job.CreatedAt),
		"updated_at":       timeValue(job.UpdatedAt),
		"started_at":       timeValue(job.StartedAt),
		"completed_at":     timeValue(job.CompletedAt),
	}
}

func conclusionValue(v actionsdb.NullCheckConclusion) any {
	if !v.Valid {
		return nil
	}
	return string(v.CheckConclusion)
}

func int8Value(v pgtype.Int8) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func int8Nullable(v pgtype.Int8) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func timeValue(v pgtype.Timestamptz) any {
	if !v.Valid {
		return nil
	}
	return v.Time.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
}
