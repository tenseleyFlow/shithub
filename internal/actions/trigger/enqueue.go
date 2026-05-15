// SPDX-License-Identifier: AGPL-3.0-or-later

package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/actions/checksync"
	"github.com/tenseleyFlow/shithub/internal/actions/concurrency"
	actionsevents "github.com/tenseleyFlow/shithub/internal/actions/events"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/checks"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// Deps wires the trigger pipeline against runtime infra.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Now    func() time.Time
}

// EnqueueParams is the input to Enqueue. The caller (trigger handler)
// has already discovered + parsed the workflow and built the typed
// event payload via internal/actions/event.
type EnqueueParams struct {
	// RepoID identifies the repo the run belongs to.
	RepoID int64

	// WorkflowFile is the repo-relative path of the parsed file
	// (e.g. ".shithub/workflows/ci.yml"). Used as part of the
	// idempotency key so two workflows in the same repo on the same
	// SHA each get their own run.
	WorkflowFile string

	// HeadSHA is the commit the run targets — for push: the after-sha;
	// for PR: the head-sha; for dispatch: the resolved-at-dispatch-time
	// SHA; for schedule: the default-branch tip at sweep time.
	HeadSHA string

	// HeadRef is the symbolic ref (e.g. "refs/heads/main",
	// "refs/heads/feature/foo"). Optional — empty when not
	// applicable (schedule).
	HeadRef string

	// EventKind is one of the four supported triggers; persisted to
	// workflow_runs.event for filtering on the runs list.
	EventKind EventKind

	// EventPayload is the canonical shithub.event payload built by the
	// matching constructor in internal/actions/event. Stored as
	// jsonb on workflow_runs.event_payload; the runner consumes via
	// expr.evalEventPath.
	EventPayload map[string]any

	// ActorUserID is the user who triggered the run (pusher,
	// PR-opener, dispatcher). Zero for schedule (system trigger).
	ActorUserID int64

	// ParentRunID is set for re-runs to chain back to the original.
	// Zero for fresh triggers.
	ParentRunID int64

	// TriggerEventID is the stable identifier of the triggering
	// event. See migration 0051's header comment for the construction
	// convention. Required (empty string is a programmer error).
	TriggerEventID string

	// Workflow is the parsed workflow. The matching jobs/steps are
	// persisted; concurrency.group is resolved against the trigger
	// context and enforced before runners can claim younger jobs.
	Workflow *workflow.Workflow

	// NeedApproval pauses runner dispatch until a maintainer approves the
	// run. The row still becomes visible in the Actions UI and check list.
	NeedApproval bool

	// ApprovalReason is stored with the approval request. It must stay
	// structural; never include event payloads, env, logs, or secrets.
	ApprovalReason string
}

// Result reports the outcome of an Enqueue call.
type Result struct {
	// RunID is the workflow_runs.id of the persisted (or already-
	// existing) row. Always non-zero when err is nil.
	RunID int64
	// RunIndex is the per-repo monotonic index (used for stable URLs).
	RunIndex int64
	// AlreadyExists is true when a prior call with the same
	// trigger_event_id had already created the run; in this case the
	// child jobs/steps/check_runs were NOT re-inserted. Caller can
	// log + move on.
	AlreadyExists bool
	// CheckRunIDs is the list of check_runs.id rows created (or
	// looked up via ExternalID) for the workflow's jobs. Order
	// matches Workflow.Jobs declaration order.
	CheckRunIDs []int64
	// Skipped is true when the workflow was matched but not enqueued
	// because the operator disabled it via the workflow_disabled
	// table. RunID/RunIndex/CheckRunIDs are zero in this case; the
	// caller's only sensible response is to log + move on.
	Skipped bool
}

var ErrActionsMinutesQuotaExceeded = errors.New("trigger: actions minutes quota exceeded")

// Enqueue persists a matched workflow as a queued run with all its
// jobs + steps in one transaction, then creates one check_run per
// job (idempotent via ExternalID) outside the tx so the run+jobs+
// steps remain atomic but check_run drift can be reconciled.
//
// On conflict (the trigger_event_id has been used for this
// workflow_file before), returns Result{AlreadyExists: true} after
// looking up the existing run — the handler treats that as success.
//
// On any other error, the inner tx is rolled back; no rows persist.
func Enqueue(ctx context.Context, deps Deps, p EnqueueParams) (Result, error) {
	if err := validateParams(&p); err != nil {
		return Result{}, err
	}
	// Honour the per-workflow disable flag (§13 REST). A disabled
	// workflow's events get matched and reach here, then bail out
	// before any run/jobs/check_runs rows are written. Re-enabling
	// (DELETE the row) resumes triggering as normal.
	disabled, err := actionsdb.New().IsWorkflowDisabled(ctx, deps.Pool, actionsdb.IsWorkflowDisabledParams{
		RepoID:       p.RepoID,
		WorkflowFile: p.WorkflowFile,
	})
	if err != nil {
		return Result{}, fmt.Errorf("trigger: check disabled: %w", err)
	}
	if disabled {
		return Result{Skipped: true}, nil
	}
	if err := enforceActionsMinutesQuota(ctx, deps, p.RepoID); err != nil {
		return Result{}, err
	}
	concurrencyResolution, err := concurrency.Resolve(concurrency.ResolveInput{
		Workflow:     p.Workflow,
		EventPayload: p.EventPayload,
		HeadSHA:      p.HeadSHA,
		HeadRef:      p.HeadRef,
	})
	if err != nil {
		return Result{}, fmt.Errorf("trigger: concurrency: %w", err)
	}

	q := actionsdb.New()

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("trigger: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// run_index is per-repo; lookup MAX+1. Concurrent inserts on the
	// same repo race here, but the (repo_id, run_index) UNIQUE catches
	// it as a unique-violation and the caller (worker handler) retries.
	// Realistic concurrency on a single repo's triggers is low for v1.
	runIndex, err := q.NextRunIndexForRepo(ctx, tx, p.RepoID)
	if err != nil {
		return Result{}, fmt.Errorf("trigger: next run index: %w", err)
	}

	payloadBytes, err := json.Marshal(p.EventPayload)
	if err != nil {
		return Result{}, fmt.Errorf("trigger: marshal event payload: %w", err)
	}

	run, err := q.EnqueueWorkflowRun(ctx, tx, actionsdb.EnqueueWorkflowRunParams{
		RepoID:           p.RepoID,
		RunIndex:         runIndex,
		WorkflowFile:     p.WorkflowFile,
		WorkflowName:     p.Workflow.Name,
		HeadSha:          p.HeadSHA,
		HeadRef:          p.HeadRef,
		Event:            actionsdb.WorkflowRunEvent(p.EventKind),
		EventPayload:     payloadBytes,
		ActorUserID:      pgInt8(p.ActorUserID),
		ParentRunID:      pgInt8(p.ParentRunID),
		ConcurrencyGroup: concurrencyResolution.Group,
		NeedApproval:     p.NeedApproval,
		TriggerEventID:   p.TriggerEventID,
	})
	if err != nil {
		// pgx.ErrNoRows = ON CONFLICT DO NOTHING fired. Lookup the
		// existing row so the handler has a stable RunID to log.
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.Commit(ctx); err != nil {
				return Result{}, fmt.Errorf("trigger: commit empty tx: %w", err)
			}
			committed = true
			existing, lookupErr := lookupExistingRun(ctx, deps.Pool, p)
			if lookupErr != nil {
				return Result{}, fmt.Errorf("trigger: existing-run lookup: %w", lookupErr)
			}
			metrics.ActionsRunsEnqueuedTotal.WithLabelValues(string(p.EventKind), "already_exists").Inc()
			return Result{
				RunID:         existing.ID,
				RunIndex:      existing.RunIndex,
				AlreadyExists: true,
			}, nil
		}
		return Result{}, fmt.Errorf("trigger: insert run: %w", err)
	}
	if err := actionsevents.EmitRunTx(ctx, tx, run, actionsevents.ActionQueued); err != nil {
		return Result{}, fmt.Errorf("trigger: emit run queued event: %w", err)
	}
	if p.NeedApproval {
		if _, err := q.InsertWorkflowRunApproval(ctx, tx, actionsdb.InsertWorkflowRunApprovalParams{
			RunID:           run.ID,
			RequestedReason: p.ApprovalReason,
		}); err != nil {
			return Result{}, fmt.Errorf("trigger: insert approval request: %w", err)
		}
	}

	// Persist child jobs + their steps. Order in Workflow.Jobs is YAML
	// document order, which we preserve via job_index.
	jobIDs := make([]int64, len(p.Workflow.Jobs))
	for i, j := range p.Workflow.Jobs {
		needs := j.Needs
		if needs == nil {
			// Postgres text[] doesn't accept Go nil — use empty slice.
			needs = []string{}
		}
		permsJSON, err := marshalPermissions(j.Permissions)
		if err != nil {
			return Result{}, fmt.Errorf("trigger: marshal permissions for job %s: %w", j.Key, err)
		}
		envJSON, err := marshalEnv(j.Env)
		if err != nil {
			return Result{}, fmt.Errorf("trigger: marshal env for job %s: %w", j.Key, err)
		}
		job, err := q.InsertWorkflowJob(ctx, tx, actionsdb.InsertWorkflowJobParams{
			RunID:          run.ID,
			JobIndex:       int32(i),
			JobKey:         j.Key,
			JobName:        j.Name,
			RunsOn:         j.RunsOn,
			NeedsJobs:      needs,
			IfExpr:         j.If,
			TimeoutMinutes: int32(j.TimeoutMinutes),
			Permissions:    permsJSON,
			JobEnv:         envJSON,
		})
		if err != nil {
			return Result{}, fmt.Errorf("trigger: insert job %s: %w", j.Key, err)
		}
		jobIDs[i] = job.ID
		if err := actionsevents.EmitJobTx(ctx, tx, run, job, actionsevents.ActionQueued); err != nil {
			return Result{}, fmt.Errorf("trigger: emit job queued event for %s: %w", j.Key, err)
		}

		for si, s := range j.Steps {
			stepEnvJSON, err := marshalEnv(s.Env)
			if err != nil {
				return Result{}, fmt.Errorf("trigger: marshal step env: %w", err)
			}
			stepWithJSON, err := marshalEnv(s.With)
			if err != nil {
				return Result{}, fmt.Errorf("trigger: marshal step with: %w", err)
			}
			if _, err := q.InsertWorkflowStep(ctx, tx, actionsdb.InsertWorkflowStepParams{
				JobID:            job.ID,
				StepIndex:        int32(si),
				StepID:           s.ID,
				StepName:         s.Name,
				IfExpr:           s.If,
				RunCommand:       s.Run,
				UsesAlias:        s.Uses,
				WorkingDirectory: s.WorkingDirectory,
				StepEnv:          stepEnvJSON,
				ContinueOnError:  s.ContinueOnError,
				StepWith:         stepWithJSON,
			}); err != nil {
				return Result{}, fmt.Errorf("trigger: insert step %d for job %s: %w", si, j.Key, err)
			}
		}
	}

	concurrencyResult, err := concurrency.Enforce(ctx, q, tx, concurrency.EnforceParams{
		Run:              run,
		CancelInProgress: concurrencyResolution.CancelInProgress,
	})
	if err != nil {
		return Result{}, fmt.Errorf("trigger: enforce concurrency: %w", err)
	}
	if err := emitConcurrencyCancelEvents(ctx, tx, q, concurrencyResult.CancelledJobs); err != nil {
		return Result{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("trigger: commit run tx: %w", err)
	}
	committed = true

	if len(concurrencyResult.CancelledJobs) > 0 {
		metrics.ActionsJobsCancelledTotal.WithLabelValues(concurrency.CancelReason).Add(float64(len(concurrencyResult.CancelledJobs)))
		checksync.ChangedJobs(ctx, checksync.Deps{Pool: deps.Pool, Logger: deps.Logger}, concurrencyResult.CancelledJobs)
	} else if !concurrencyResolution.CancelInProgress && len(concurrencyResult.BlockingRuns) > 0 {
		metrics.ActionsConcurrencyQueuedTotal.Inc()
	}

	// check_run rows: separate concern, post-commit so the run+jobs+
	// steps are durable before we touch a different subsystem. ExternalID
	// idempotency means a retry of just this phase converges cleanly.
	checkRunIDs := make([]int64, 0, len(p.Workflow.Jobs))
	checkDetailsURL := ""
	if ownerRow, err := reposdb.New().GetRepoOwnerUsernameByID(ctx, deps.Pool, p.RepoID); err == nil {
		if ownerSlug, ok := repoOwnerSlugString(ownerRow.OwnerUsername); ok {
			checkDetailsURL = fmt.Sprintf("/%s/%s/actions/runs/%d", url.PathEscape(ownerSlug), url.PathEscape(ownerRow.RepoName), run.RunIndex)
		}
	} else {
		deps.Logger.WarnContext(ctx, "trigger: load repo owner for check_run details_url", "repo_id", p.RepoID, "error", err)
	}
	for i, j := range p.Workflow.Jobs {
		extID := fmt.Sprintf("workflow_run:%d:job:%s", run.ID, j.Key)
		name := j.Name
		if name == "" {
			name = j.Key
		}
		cr, err := checks.Create(ctx, checks.Deps{Pool: deps.Pool, Logger: deps.Logger}, checks.CreateParams{
			RepoID:     p.RepoID,
			HeadSHA:    p.HeadSHA,
			AppSlug:    "shithub-actions",
			Name:       name,
			Status:     "queued",
			DetailsURL: checkDetailsURL,
			ExternalID: extID,
		})
		if err != nil {
			// Don't roll back the run on check_run failure — log and
			// continue. The run is queued and visible; checks can
			// reconcile via a future retry of just this loop.
			deps.Logger.WarnContext(ctx, "trigger: check_run create failed",
				"run_id", run.ID, "job_key", j.Key, "error", err)
			continue
		}
		checkRunIDs = append(checkRunIDs, cr.ID)
		_ = jobIDs[i] // job_id linkage to check_run lands in S41c when the runner consumes both
	}

	metrics.ActionsRunsEnqueuedTotal.WithLabelValues(string(p.EventKind), "fresh").Inc()
	return Result{
		RunID:       run.ID,
		RunIndex:    run.RunIndex,
		CheckRunIDs: checkRunIDs,
	}, nil
}

func enforceActionsMinutesQuota(ctx context.Context, deps Deps, repoID int64) error {
	repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, repoID)
	if err != nil {
		return fmt.Errorf("trigger: load repo for quota: %w", err)
	}
	if !repo.OwnerOrgID.Valid {
		return nil
	}
	now := time.Now().UTC()
	if deps.Now != nil {
		now = deps.Now().UTC()
	}
	periodStart, periodEnd := orgbilling.MonthlyUsagePeriod(now)
	counters, err := orgbilling.RecalculateOrgUsageCounters(ctx, orgbilling.Deps{Pool: deps.Pool}, repo.OwnerOrgID.Int64, periodStart, periodEnd)
	if err != nil {
		return fmt.Errorf("trigger: recalculate actions quota: %w", err)
	}
	set, err := entitlements.ForOrg(ctx, entitlements.Deps{Pool: deps.Pool, Now: func() time.Time { return now }}, repo.OwnerOrgID.Int64)
	if err != nil {
		return fmt.Errorf("trigger: actions quota entitlements: %w", err)
	}
	limit, err := set.Limit(entitlements.LimitOrgActionsMinutesQuota)
	if err != nil {
		return fmt.Errorf("trigger: actions quota limit: %w", err)
	}
	if !limit.Defined || limit.Unlimited || limit.Value <= 0 {
		return nil
	}
	if counters.ActionsMinutesUsed >= limit.Value {
		return fmt.Errorf("%w: org_id=%d used=%d limit=%d",
			ErrActionsMinutesQuotaExceeded,
			repo.OwnerOrgID.Int64,
			counters.ActionsMinutesUsed,
			limit.Value)
	}
	return nil
}

func validateParams(p *EnqueueParams) error {
	if p.RepoID == 0 {
		return errors.New("trigger: RepoID required")
	}
	if p.WorkflowFile == "" {
		return errors.New("trigger: WorkflowFile required")
	}
	if p.HeadSHA == "" {
		return errors.New("trigger: HeadSHA required")
	}
	if p.TriggerEventID == "" {
		return errors.New("trigger: TriggerEventID required (empty would bypass idempotency)")
	}
	if p.Workflow == nil {
		return errors.New("trigger: Workflow required")
	}
	if len(p.Workflow.Jobs) == 0 {
		return errors.New("trigger: workflow has no jobs")
	}
	switch p.EventKind {
	case EventPush, EventPullRequest, EventSchedule, EventWorkflowDispatch:
	default:
		return fmt.Errorf("trigger: unsupported event kind %q", p.EventKind)
	}
	return nil
}

// lookupExistingRun finds a workflow_run by the trigger_event_id key
// after EnqueueWorkflowRun reported a conflict. Done outside the
// inner tx (which we committed empty) so the caller has a stable
// RunID to surface.
func lookupExistingRun(ctx context.Context, pool *pgxpool.Pool, p EnqueueParams) (actionsdb.WorkflowRun, error) {
	q := actionsdb.New()
	rows, err := q.LookupWorkflowRunByTriggerEvent(ctx, pool, actionsdb.LookupWorkflowRunByTriggerEventParams{
		RepoID:         p.RepoID,
		WorkflowFile:   p.WorkflowFile,
		TriggerEventID: p.TriggerEventID,
	})
	if err != nil {
		return actionsdb.WorkflowRun{}, err
	}
	return rows, nil
}

func emitConcurrencyCancelEvents(
	ctx context.Context,
	tx pgx.Tx,
	q *actionsdb.Queries,
	jobs []actionsdb.WorkflowJob,
) error {
	if len(jobs) == 0 {
		return nil
	}
	emittedRun := map[int64]struct{}{}
	for _, job := range jobs {
		run, err := q.GetWorkflowRunByID(ctx, tx, job.RunID)
		if err != nil {
			return fmt.Errorf("trigger: load concurrency-cancelled run: %w", err)
		}
		if job.Status == actionsdb.WorkflowJobStatusCancelled {
			if err := actionsevents.EmitJobTx(ctx, tx, run, job, actionsevents.ActionCancelled); err != nil {
				return fmt.Errorf("trigger: emit concurrency job cancelled event: %w", err)
			}
		}
		if _, ok := emittedRun[run.ID]; ok {
			continue
		}
		if workflowRunTerminal(run.Status) {
			if err := actionsevents.EmitRunTx(ctx, tx, run, runTerminalAction(run)); err != nil {
				return fmt.Errorf("trigger: emit concurrency run terminal event: %w", err)
			}
			emittedRun[run.ID] = struct{}{}
		}
	}
	return nil
}

func workflowRunTerminal(status actionsdb.WorkflowRunStatus) bool {
	return status == actionsdb.WorkflowRunStatusCompleted || status == actionsdb.WorkflowRunStatusCancelled
}

func runTerminalAction(run actionsdb.WorkflowRun) string {
	if run.Status == actionsdb.WorkflowRunStatusCancelled {
		return actionsevents.ActionCancelled
	}
	return actionsevents.ActionCompleted
}

func pgInt8(v int64) pgtype.Int8 {
	return pgtype.Int8{Int64: v, Valid: v != 0}
}

func repoOwnerSlugString(v any) (string, bool) {
	switch s := v.(type) {
	case string:
		return s, s != ""
	case []byte:
		return string(s), len(s) > 0
	default:
		return "", false
	}
}

// marshalEnv encodes a workflow.Value-keyed map to jsonb-friendly
// {string: string} for the step_env / job_env / step_with columns.
// We don't persist the parser-side struct directly — the runner only
// needs the resolved string value.
func marshalEnv(m map[string]workflow.Value) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.Raw
	}
	return json.Marshal(out)
}

// marshalPermissions flattens a workflow.Permissions into the jsonb
// shape S41c will consume. v1 just stores Mode + Per — no resolution.
func marshalPermissions(p workflow.Permissions) ([]byte, error) {
	if p.Mode == "" && len(p.Per) == 0 {
		return []byte("{}"), nil
	}
	out := map[string]any{}
	if p.Mode != "" {
		out["mode"] = p.Mode
	}
	if len(p.Per) > 0 {
		per := make(map[string]string, len(p.Per))
		for k, v := range p.Per {
			per[k] = string(v)
		}
		out["per"] = per
	}
	return json.Marshal(out)
}
