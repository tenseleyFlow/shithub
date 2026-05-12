// SPDX-License-Identifier: AGPL-3.0-or-later

package trigger_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const enqFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type enqFx struct {
	pool   *pgxpool.Pool
	deps   trigger.Deps
	repoID int64
	userID int64
}

func setupEnq(t *testing.T) enqFx {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: enqFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return enqFx{
		pool:   pool,
		deps:   trigger.Deps{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		repoID: repo.ID,
		userID: user.ID,
	}
}

// fixtureWorkflow returns a small valid Workflow with one job + two
// steps. Used by every enqueue test.
func fixtureWorkflow(t *testing.T) *workflow.Workflow {
	t.Helper()
	return workflowFromYAML(t, `name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo hello
`)
}

func concurrencyWorkflow(t *testing.T, group string, cancelInProgress bool) *workflow.Workflow {
	t.Helper()
	return workflowFromYAML(t, fmt.Sprintf(`name: ci
on: push
concurrency:
  group: "%s"
  cancel-in-progress: %t
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo hello
`, group, cancelInProgress))
}

func workflowFromYAML(t *testing.T, src string) *workflow.Workflow {
	t.Helper()
	w, diags, err := workflow.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	for _, d := range diags {
		if d.Severity == workflow.Error {
			t.Fatalf("unexpected diagnostic: %v", d)
		}
	}
	return w
}

func TestEnqueue_HappyPath(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	res, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("a", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:42",
		Workflow:       fixtureWorkflow(t),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if res.RunID == 0 || res.RunIndex == 0 || res.AlreadyExists {
		t.Errorf("expected fresh run, got %+v", res)
	}
	// One job, so one check_run.
	if len(res.CheckRunIDs) != 1 {
		t.Errorf("expected 1 check_run, got %d", len(res.CheckRunIDs))
	}

	// Verify rows landed.
	q := actionsdb.New()
	run, err := q.GetWorkflowRunByID(ctx, f.pool, res.RunID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if run.TriggerEventID != "push:42" {
		t.Errorf("trigger_event_id: got %q want push:42", run.TriggerEventID)
	}
	if run.Status != actionsdb.WorkflowRunStatusQueued {
		t.Errorf("status: got %s want queued", run.Status)
	}
	assertDomainEventCounts(t, f.pool, f.repoID, map[string]int64{
		"workflow_run": 1,
		"workflow_job": 1,
	})
}

func TestEnqueue_ResolvesConcurrencyGroupExpression(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	res, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("a", 40),
		HeadRef:        "refs/heads/feature",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/feature"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:concurrency-expr",
		Workflow:       concurrencyWorkflow(t, "branch-${{ shithub.ref }}", false),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	run, err := actionsdb.New().GetWorkflowRunByID(ctx, f.pool, res.RunID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if run.ConcurrencyGroup != "branch-refs/heads/feature" {
		t.Fatalf("concurrency_group: got %q", run.ConcurrencyGroup)
	}
}

func TestEnqueue_CancelInProgressCancelsOlderQueuedRun(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	q := actionsdb.New()
	first, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("a", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:concurrency-cancel-1",
		Workflow:       concurrencyWorkflow(t, "${{ shithub.ref }}", false),
	})
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	second, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("b", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:concurrency-cancel-2",
		Workflow:       concurrencyWorkflow(t, "${{ shithub.ref }}", true),
	})
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	oldRun, err := q.GetWorkflowRunByID(ctx, f.pool, first.RunID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID old: %v", err)
	}
	if oldRun.Status != actionsdb.WorkflowRunStatusCompleted ||
		!oldRun.Conclusion.Valid ||
		oldRun.Conclusion.CheckConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("old run not cancelled: %+v", oldRun)
	}
	oldJobs, err := q.ListJobsForRun(ctx, f.pool, first.RunID)
	if err != nil {
		t.Fatalf("ListJobsForRun old: %v", err)
	}
	if len(oldJobs) != 1 || oldJobs[0].Status != actionsdb.WorkflowJobStatusCancelled {
		t.Fatalf("old jobs not cancelled: %+v", oldJobs)
	}
	oldSteps, err := q.ListStepsForJob(ctx, f.pool, oldJobs[0].ID)
	if err != nil {
		t.Fatalf("ListStepsForJob old: %v", err)
	}
	for _, step := range oldSteps {
		if step.Status != actionsdb.WorkflowStepStatusCancelled {
			t.Fatalf("step %d status: got %s want cancelled", step.ID, step.Status)
		}
	}
	newRun, err := q.GetWorkflowRunByID(ctx, f.pool, second.RunID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID new: %v", err)
	}
	if newRun.Status != actionsdb.WorkflowRunStatusQueued {
		t.Fatalf("new run status: got %s want queued", newRun.Status)
	}
	assertDomainEventCounts(t, f.pool, f.repoID, map[string]int64{
		"workflow_run": 3,
		"workflow_job": 3,
	})
}

func assertDomainEventCounts(t *testing.T, db actionsdb.DBTX, repoID int64, want map[string]int64) {
	t.Helper()
	for kind, n := range want {
		var got int64
		if err := db.QueryRow(context.Background(),
			`SELECT count(*) FROM domain_events WHERE repo_id = $1 AND kind = $2`,
			repoID, kind,
		).Scan(&got); err != nil {
			t.Fatalf("count domain events %s: %v", kind, err)
		}
		if got != n {
			t.Fatalf("domain_events[%s] = %d, want %d", kind, got, n)
		}
	}
}

func TestClaimQueuedWorkflowJob_BlocksYoungerConcurrencyRun(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	q := actionsdb.New()
	first, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("c", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:concurrency-block-1",
		Workflow:       concurrencyWorkflow(t, "${{ shithub.ref }}", false),
	})
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	second, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("d", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:concurrency-block-2",
		Workflow:       concurrencyWorkflow(t, "${{ shithub.ref }}", false),
	})
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	runner, err := q.InsertRunner(ctx, f.pool, actionsdb.InsertRunnerParams{
		Name:     "runner-block",
		Labels:   []string{"ubuntu-latest"},
		Capacity: 2,
	})
	if err != nil {
		t.Fatalf("InsertRunner: %v", err)
	}
	claimed, err := q.ClaimQueuedWorkflowJob(ctx, f.pool, actionsdb.ClaimQueuedWorkflowJobParams{
		Labels:   []string{"ubuntu-latest"},
		RunnerID: runner.ID,
	})
	if err != nil {
		t.Fatalf("first ClaimQueuedWorkflowJob: %v", err)
	}
	if claimed.RunID != first.RunID {
		t.Fatalf("claimed run: got %d want first run %d", claimed.RunID, first.RunID)
	}
	_, err = q.ClaimQueuedWorkflowJob(ctx, f.pool, actionsdb.ClaimQueuedWorkflowJobParams{
		Labels:   []string{"ubuntu-latest"},
		RunnerID: runner.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim error: got %v want pgx.ErrNoRows", err)
	}
	changed, err := q.RequestWorkflowRunCancel(ctx, f.pool, first.RunID)
	if err != nil {
		t.Fatalf("RequestWorkflowRunCancel: %v", err)
	}
	if len(changed) != 1 || !changed[0].CancelRequested {
		t.Fatalf("cancel request did not release blocker: %+v", changed)
	}
	released, err := q.ClaimQueuedWorkflowJob(ctx, f.pool, actionsdb.ClaimQueuedWorkflowJobParams{
		Labels:   []string{"ubuntu-latest"},
		RunnerID: runner.ID,
	})
	if err != nil {
		t.Fatalf("claim after cancel request: %v", err)
	}
	if released.RunID != second.RunID {
		t.Fatalf("released claim run: got %d want second run %d", released.RunID, second.RunID)
	}
}

func TestEnqueue_IdempotentSecondCall(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	params := trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("b", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:99",
		Workflow:       fixtureWorkflow(t),
	}
	first, err := trigger.Enqueue(ctx, f.deps, params)
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if first.AlreadyExists {
		t.Fatal("first call should not be AlreadyExists")
	}
	second, err := trigger.Enqueue(ctx, f.deps, params)
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if !second.AlreadyExists {
		t.Errorf("second call should be AlreadyExists")
	}
	if second.RunID != first.RunID {
		t.Errorf("second call must return the SAME run id; got %d vs %d", second.RunID, first.RunID)
	}
	// Verify only one row exists.
	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM workflow_runs WHERE repo_id=$1 AND trigger_event_id=$2`,
		f.repoID, "push:99").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 workflow_run, got %d", count)
	}
}

func TestEnqueue_DifferentTriggerEventIDsDoNotCollide(t *testing.T) {
	// Re-runs explicitly produce a different trigger_event_id. Verify
	// they're allowed alongside the original.
	f := setupEnq(t)
	ctx := context.Background()
	base := trigger.EnqueueParams{
		RepoID:       f.repoID,
		WorkflowFile: ".shithub/workflows/ci.yml",
		HeadSHA:      strings.Repeat("c", 40),
		HeadRef:      "refs/heads/trunk",
		EventKind:    trigger.EventPush,
		EventPayload: map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:  f.userID,
		Workflow:     fixtureWorkflow(t),
	}
	first := base
	first.TriggerEventID = "push:1"
	res1, err := trigger.Enqueue(ctx, f.deps, first)
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	rerun := base
	rerun.TriggerEventID = "rerun:" + strings.Repeat("c", 40) + ":xyz"
	rerun.ParentRunID = res1.RunID
	res2, err := trigger.Enqueue(ctx, f.deps, rerun)
	if err != nil {
		t.Fatalf("rerun Enqueue: %v", err)
	}
	if res2.AlreadyExists {
		t.Error("rerun should produce a new run, not AlreadyExists")
	}
	if res2.RunID == res1.RunID {
		t.Error("rerun must have a different RunID")
	}
}

func TestEnqueue_EmptyTriggerEventIDIsRejected(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	_, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("d", 40),
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{},
		Workflow:       fixtureWorkflow(t),
		TriggerEventID: "",
	})
	if err == nil {
		t.Fatal("empty TriggerEventID should error — would silently bypass idempotency")
	}
}

func TestEnqueue_RunIndexIsPerRepoMonotonic(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	mk := func(triggerID string) trigger.EnqueueParams {
		return trigger.EnqueueParams{
			RepoID:         f.repoID,
			WorkflowFile:   ".shithub/workflows/ci.yml",
			HeadSHA:        strings.Repeat("e", 40),
			EventKind:      trigger.EventPush,
			EventPayload:   map[string]any{},
			ActorUserID:    f.userID,
			TriggerEventID: triggerID,
			Workflow:       fixtureWorkflow(t),
		}
	}
	r1, err := trigger.Enqueue(ctx, f.deps, mk("push:101"))
	if err != nil {
		t.Fatalf("Enqueue 1: %v", err)
	}
	r2, err := trigger.Enqueue(ctx, f.deps, mk("push:102"))
	if err != nil {
		t.Fatalf("Enqueue 2: %v", err)
	}
	if r2.RunIndex != r1.RunIndex+1 {
		t.Errorf("run_index not monotonic; got r1=%d r2=%d", r1.RunIndex, r2.RunIndex)
	}
}

// TestEnqueue_ChildRowsExist confirms that the per-tx run+jobs+steps
// insertion lands all three layers atomically (i.e., we don't end up
// with an orphan run with no jobs).
func TestEnqueue_ChildRowsExist(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	res, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("f", 40),
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{},
		ActorUserID:    f.userID,
		TriggerEventID: "push:200",
		Workflow:       fixtureWorkflow(t),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	q := actionsdb.New()
	jobs, err := q.ListJobsForRun(ctx, f.pool, res.RunID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	steps, err := q.ListStepsForJob(ctx, f.pool, jobs[0].ID)
	if err != nil {
		t.Fatalf("ListStepsForJob: %v", err)
	}
	if len(steps) != 2 {
		t.Errorf("expected 2 steps (uses + run), got %d", len(steps))
	}
}

// TestEnqueue_ConflictDetectsExistingRun exercises the lookup path
// the conflict branch takes: confirm pgx.ErrNoRows from the INSERT
// is correctly translated into AlreadyExists rather than bubbling
// out as an error.
func TestEnqueue_ConflictDetectsExistingRun(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	params := trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("9", 40),
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{},
		ActorUserID:    f.userID,
		TriggerEventID: "push:conflict",
		Workflow:       fixtureWorkflow(t),
	}
	if _, err := trigger.Enqueue(ctx, f.deps, params); err != nil {
		t.Fatalf("first: %v", err)
	}
	res, err := trigger.Enqueue(ctx, f.deps, params)
	if err != nil {
		// Ensure the conflict path doesn't surface as ErrNoRows by
		// accident.
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("conflict path should NOT return pgx.ErrNoRows; expected AlreadyExists")
		}
		t.Fatalf("conflict: %v", err)
	}
	if !res.AlreadyExists {
		t.Error("expected AlreadyExists on second call")
	}
}
