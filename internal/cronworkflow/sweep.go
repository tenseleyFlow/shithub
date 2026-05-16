// SPDX-License-Identifier: AGPL-3.0-or-later

package cronworkflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/actions/dispatch"
	"github.com/tenseleyFlow/shithub/internal/actions/event"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	crondb "github.com/tenseleyFlow/shithub/internal/cronworkflow/sqlc"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// SweepBatch caps how many due rows one sweep tick consumes. Bounds
// the worker job's wall-clock + holds locks for a bounded duration.
// The handler self-throttles by re-enqueueing when a batch fills.
const SweepBatch = 50

// FireDeps wires the per-row fire path. The sweep job receives one
// FireDeps and reuses it across the whole batch.
type FireDeps struct {
	Pool           *pgxpool.Pool
	Logger         *slog.Logger
	RepoFS         *storage.RepoFS
	BillingEnforce config.EnforceConfig
	// Now is plumbed for tests so the sweep advances against a fixed
	// clock rather than wall-clock time. Production uses time.Now.
	Now func() time.Time
}

// SweepOnce claims up to SweepBatch due rows and fires each one.
// Returns the number of rows processed (regardless of fire outcome —
// skipped rows count too). Caller (the worker handler) re-enqueues
// itself when this returns SweepBatch.
//
// Rows are claimed with FOR UPDATE SKIP LOCKED so concurrent sweep
// workers don't double-fire. Each fire runs inside its own short tx
// so a slow Enqueue (which itself opens transactions) doesn't hold
// the batch's locks.
func SweepOnce(ctx context.Context, deps FireDeps) (int, error) {
	if deps.Pool == nil {
		return 0, errors.New("cronworkflow: SweepOnce needs Pool")
	}
	if deps.RepoFS == nil {
		return 0, errors.New("cronworkflow: SweepOnce needs RepoFS")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	// FOR UPDATE SKIP LOCKED inside an outer tx; we advance each row
	// in its own sub-tx by calling AdvanceCronDispatch on the pool
	// (so the lock-holding outer tx is released as soon as the SELECT
	// resolves and rows are visited).
	tx, err := deps.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("cronworkflow: sweep begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed on success path below

	q := crondb.New()
	rows, err := q.ClaimDueCronDispatches(ctx, tx, SweepBatch)
	if err != nil {
		return 0, fmt.Errorf("cronworkflow: claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("cronworkflow: claim commit: %w", err)
	}

	processed := 0
	for _, row := range rows {
		fireRow(ctx, deps, row, now)
		processed++
	}
	return processed, nil
}

// fireRow runs the dispatch + advance for one row. Always advances
// next_fire_at — even on skip — to avoid the sweep spinning on the
// same row every tick.
func fireRow(ctx context.Context, deps FireDeps, row crondb.UserCronDispatch, now func() time.Time) {
	q := crondb.New()
	status, fireErr := dispatchOne(ctx, deps, row)

	// Always advance from now() — missed ticks are dropped (matches
	// GitHub Actions schedule semantics). A daily schedule whose
	// worker missed yesterday's fire jumps to today's, not loops to
	// catch up. The sweep guarantees at-most-one fire per due row;
	// the "every missed tick fires" model would amplify a worker
	// outage into a burst on recovery.
	base := now()
	next, err := NextTick(row.CronExpr, base)
	if err != nil {
		// Stored expression doesn't parse — disable to prevent loop.
		if logger := deps.Logger; logger != nil {
			logger.WarnContext(ctx, "cronworkflow: stored expr invalid; disabling",
				"dispatch_id", row.ID, "expr", row.CronExpr, "error", err)
		}
		_ = q.DisableCronDispatch(ctx, deps.Pool, row.ID)
		return
	}
	var errText pgtype.Text
	if fireErr != "" {
		errText = pgtype.Text{String: fireErr, Valid: true}
	}
	if err := q.AdvanceCronDispatch(ctx, deps.Pool, crondb.AdvanceCronDispatchParams{
		ID:             row.ID,
		NextFireAt:     pgtype.Timestamptz{Time: next, Valid: true},
		LastFireStatus: status,
		LastFireError:  errText,
	}); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "cronworkflow: advance failed",
			"dispatch_id", row.ID, "error", err)
	}
}

// dispatchOne does the work of one fire: entitlement check, resolve
// gitDir, ResolveRefOID, ReadBlobBytes, workflow.Parse, trigger.Enqueue.
// Returns the status + an optional error string for the row's
// last_fire_error column.
//
// On any skip path the function logs at info+ and returns a skip
// status — the sweep still advances next_fire_at to keep the cadence.
func dispatchOne(ctx context.Context, deps FireDeps, row crondb.UserCronDispatch) (crondb.UserCronDispatchLastStatus, string) {
	logger := deps.Logger
	// Entitlement gate. Report-only: log + proceed. Enforce: skip.
	allowed, decision, err := checkEntitlement(ctx, deps.Pool, row.UserID)
	if err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "cronworkflow: entitlement check failed",
				"user_id", row.UserID, "error", err)
		}
		return crondb.UserCronDispatchLastStatusError, err.Error()
	}
	if !allowed && logger != nil {
		mode := "report_only"
		if deps.BillingEnforce.UserCronWorkflowDispatch {
			mode = "enforce"
		}
		logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", orgbilling.PrincipalForUser(row.UserID).String(),
			"principal_kind", string(orgbilling.SubjectKindUser),
			"principal_id", row.UserID,
			"feature", string(entitlements.FeatureCronWorkflowDispatch),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "cron-workflow-dispatch")
	}
	if !allowed && deps.BillingEnforce.UserCronWorkflowDispatch {
		return crondb.UserCronDispatchLastStatusSkippedEntitlement, ""
	}

	// Resolve repo + on-disk gitDir.
	repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, row.RepoID)
	if err != nil {
		return crondb.UserCronDispatchLastStatusSkippedMissingRef, "repo lookup: " + err.Error()
	}
	gitDir, err := repoGitDir(ctx, deps, &repo)
	if err != nil {
		return crondb.UserCronDispatchLastStatusError, "gitDir: " + err.Error()
	}

	branch := trimRefsHeads(row.Ref)
	headSHA, err := repogit.ResolveRefOID(ctx, gitDir, branch)
	if err != nil {
		return crondb.UserCronDispatchLastStatusSkippedMissingRef,
			fmt.Sprintf("resolve ref %q: %v", branch, err)
	}

	bytes, err := repogit.ReadBlobBytes(ctx, gitDir, headSHA, row.WorkflowFile,
		int64(workflow.MaxWorkflowFileBytes))
	if err != nil || len(bytes) == 0 {
		return crondb.UserCronDispatchLastStatusSkippedMissingWorkflow,
			fmt.Sprintf("workflow %q not found at %s", row.WorkflowFile, headSHA)
	}

	wf, diags, err := workflow.Parse(bytes)
	if err != nil {
		return crondb.UserCronDispatchLastStatusSkippedParseError, "parse: " + err.Error()
	}
	for _, d := range diags {
		if d.Severity == workflow.Error {
			return crondb.UserCronDispatchLastStatusSkippedParseError,
				"parse error diagnostic: " + d.String()
		}
	}

	// Build the event payload + (empty) inputs. Cron-fired dispatches
	// don't carry runtime inputs — the workflow runs with the defaults
	// declared in on.workflow_dispatch.inputs (resolved via dispatch.
	// NormalizeInputs).
	inputs, err := dispatch.NormalizeInputs(nil, nil)
	if err != nil {
		return crondb.UserCronDispatchLastStatusSkippedParseError,
			"normalize inputs: " + err.Error()
	}
	_ = inputs // schedule's event payload uses event.Schedule, not WorkflowDispatch

	triggerID := fmt.Sprintf("cron:%d:%s:%s", row.ID, headSHA, row.NextFireAt.Time.UTC().Format("20060102T150405Z"))
	if _, err := trigger.Enqueue(ctx, trigger.Deps{Pool: deps.Pool, Logger: logger},
		trigger.EnqueueParams{
			RepoID:         row.RepoID,
			WorkflowFile:   row.WorkflowFile,
			HeadSHA:        headSHA,
			HeadRef:        "refs/heads/" + branch,
			EventKind:      trigger.EventSchedule,
			EventPayload:   event.Schedule(),
			ActorUserID:    0, // system trigger; user-scope secret resolution still uses repo owner
			TriggerEventID: triggerID,
			Workflow:       wf,
		}); err != nil {
		return crondb.UserCronDispatchLastStatusError, "enqueue: " + err.Error()
	}
	return crondb.UserCronDispatchLastStatusFired, ""
}

// checkEntitlement is the cron-dispatch gate. Returns allowed +
// the Decision (so the caller can log the reason) + an error for
// transport-level failures.
func checkEntitlement(ctx context.Context, pool *pgxpool.Pool, userID int64) (bool, entitlements.Decision, error) {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: pool},
		orgbilling.PrincipalForUser(userID),
		entitlements.FeatureCronWorkflowDispatch)
	if err != nil {
		return false, entitlements.Decision{}, err
	}
	return decision.Allowed, decision, nil
}

// repoGitDir mirrors the resolution the API + HTML dispatch handlers
// use — owner slug + repo name → on-disk path. Inlined here to avoid
// pulling the api package into a worker dependency.
func repoGitDir(ctx context.Context, deps FireDeps, repo *reposdb.Repo) (string, error) {
	slug, err := repoOwnerSlug(ctx, deps.Pool, repo)
	if err != nil {
		return "", err
	}
	return deps.RepoFS.RepoPath(slug, repo.Name)
}

func repoOwnerSlug(ctx context.Context, pool *pgxpool.Pool, repo *reposdb.Repo) (string, error) {
	if repo.OwnerUserID.Valid {
		user, err := usersdb.New().GetUserByID(ctx, pool, repo.OwnerUserID.Int64)
		if err != nil {
			return "", err
		}
		return user.Username, nil
	}
	if repo.OwnerOrgID.Valid {
		org, err := orgsdb.New().GetOrgByID(ctx, pool, repo.OwnerOrgID.Int64)
		if err != nil {
			return "", err
		}
		return string(org.Slug), nil
	}
	return "", errors.New("cronworkflow: repo has no owner")
}

func trimRefsHeads(ref string) string {
	const prefix = "refs/heads/"
	if len(ref) > len(prefix) && ref[:len(prefix)] == prefix {
		return ref[len(prefix):]
	}
	return ref
}
