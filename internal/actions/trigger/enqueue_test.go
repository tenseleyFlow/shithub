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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/billing"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/orgs"
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
	orgID  int64
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

func setupOrgEnq(t *testing.T) enqFx {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: enqFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	org, err := orgs.Create(ctx, orgs.Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme Inc", CreatedByUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
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
		orgID:  org.ID,
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
	} else {
		checkRun, err := checksdb.New().GetCheckRun(ctx, f.pool, res.CheckRunIDs[0])
		if err != nil {
			t.Fatalf("GetCheckRun: %v", err)
		}
		if checkRun.DetailsUrl != "/alice/demo/actions/runs/1" {
			t.Errorf("check details_url = %q, want local run page", checkRun.DetailsUrl)
		}
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

func TestEnqueue_PersistsJobEnvironment(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	w := workflowFromYAML(t, `name: deploy
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment:
      name: production
      url: https://deployments.example.com
    steps:
      - run: echo deploy
`)
	res, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/deploy.yml",
		HeadSHA:        strings.Repeat("b", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:deploy",
		Workflow:       w,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	jobs, err := actionsdb.New().ListJobsForRun(ctx, f.pool, res.RunID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].EnvironmentName != "production" || jobs[0].EnvironmentUrl != "https://deployments.example.com" {
		t.Fatalf("environment persisted as name=%q url=%q", jobs[0].EnvironmentName, jobs[0].EnvironmentUrl)
	}
}

func TestEnqueue_RequiresApprovalForReviewerGatedEnvironment(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	q := actionsdb.New()
	if _, err := q.UpsertRepoEnvironment(ctx, f.pool, actionsdb.UpsertRepoEnvironmentParams{
		RepoID:                   f.repoID,
		Name:                     "production",
		RequiredReviewersEnabled: true,
		PreventSelfReview:        true,
		WaitTimerMinutes:         0,
		DeploymentBranchPolicy:   actionsdb.RepoEnvironmentDeploymentBranchPolicyAll,
	}); err != nil {
		t.Fatalf("UpsertRepoEnvironment: %v", err)
	}
	w := workflowFromYAML(t, `name: deploy
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - run: echo deploy
`)
	res, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/deploy.yml",
		HeadSHA:        strings.Repeat("c", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:deploy-review",
		Workflow:       w,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	run, err := q.GetWorkflowRunByID(ctx, f.pool, res.RunID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if !run.NeedApproval {
		t.Fatalf("reviewer-gated environment did not mark run approval-pending: %+v", run)
	}
	approval, err := q.GetWorkflowRunApproval(ctx, f.pool, res.RunID)
	if err != nil {
		t.Fatalf("GetWorkflowRunApproval: %v", err)
	}
	if !strings.Contains(approval.RequestedReason, "Deployment to production requires environment approval") {
		t.Fatalf("approval reason = %q", approval.RequestedReason)
	}
}

func TestClaimQueuedWorkflowJob_DoesNotRetroactivelyDeadlockReviewerGate(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	q := actionsdb.New()
	w := workflowFromYAML(t, `name: deploy
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - run: echo deploy
`)
	res, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/deploy.yml",
		HeadSHA:        strings.Repeat("d", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:deploy-before-env-gate",
		Workflow:       w,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.UpsertRepoEnvironment(ctx, f.pool, actionsdb.UpsertRepoEnvironmentParams{
		RepoID:                   f.repoID,
		Name:                     "production",
		RequiredReviewersEnabled: true,
		PreventSelfReview:        true,
		WaitTimerMinutes:         0,
		DeploymentBranchPolicy:   actionsdb.RepoEnvironmentDeploymentBranchPolicyAll,
	}); err != nil {
		t.Fatalf("UpsertRepoEnvironment: %v", err)
	}
	runner, err := q.InsertRunner(ctx, f.pool, actionsdb.InsertRunnerParams{
		Name:     "runner-retro-env",
		Labels:   []string{"ubuntu-latest"},
		Capacity: 1,
	})
	if err != nil {
		t.Fatalf("InsertRunner: %v", err)
	}
	claimed, err := q.ClaimQueuedWorkflowJob(ctx, f.pool, actionsdb.ClaimQueuedWorkflowJobParams{
		Labels:   []string{"ubuntu-latest"},
		RunnerID: runner.ID,
	})
	if err != nil {
		t.Fatalf("ClaimQueuedWorkflowJob: %v", err)
	}
	if claimed.RunID != res.RunID {
		t.Fatalf("claimed run = %d, want %d", claimed.RunID, res.RunID)
	}
}

func TestEnqueue_BlocksOrgActionsWhenMonthlyMinutesExhausted(t *testing.T) {
	f := setupOrgEnq(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	f.deps.Now = func() time.Time { return now }
	seedCompletedActionsMinutes(t, f, now, entitlements.FreeOrgActionsMinutesQuota)

	res, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("a", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:quota-exhausted",
		Workflow:       fixtureWorkflow(t),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !res.QuotaBlocked || res.RunID == 0 || res.RunIndex == 0 {
		t.Fatalf("expected visible quota-blocked run, got %+v", res)
	}
	if len(res.CheckRunIDs) != 1 {
		t.Fatalf("quota-blocked check runs = %d, want 1", len(res.CheckRunIDs))
	}
	run, err := actionsdb.New().GetWorkflowRunByID(ctx, f.pool, res.RunID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if run.Status != actionsdb.WorkflowRunStatusCompleted ||
		!run.Conclusion.Valid ||
		run.Conclusion.CheckConclusion != actionsdb.CheckConclusionActionRequired {
		t.Fatalf("quota-blocked run status/conclusion = %s/%v", run.Status, run.Conclusion)
	}
	jobs, err := actionsdb.New().ListJobsForRun(ctx, f.pool, res.RunID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 ||
		jobs[0].Status != actionsdb.WorkflowJobStatusSkipped ||
		!jobs[0].Conclusion.Valid ||
		jobs[0].Conclusion.CheckConclusion != actionsdb.CheckConclusionActionRequired {
		t.Fatalf("quota-blocked jobs = %+v", jobs)
	}
	steps, err := actionsdb.New().ListStepsForJob(ctx, f.pool, jobs[0].ID)
	if err != nil {
		t.Fatalf("ListStepsForJob: %v", err)
	}
	for _, step := range steps {
		if step.Status != actionsdb.WorkflowStepStatusSkipped ||
			!step.Conclusion.Valid ||
			step.Conclusion.CheckConclusion != actionsdb.CheckConclusionActionRequired {
			t.Fatalf("quota-blocked step = %+v", step)
		}
	}
	checkRun, err := checksdb.New().GetCheckRun(ctx, f.pool, res.CheckRunIDs[0])
	if err != nil {
		t.Fatalf("GetCheckRun: %v", err)
	}
	if checkRun.Status != checksdb.CheckStatusCompleted ||
		!checkRun.Conclusion.Valid ||
		checkRun.Conclusion.CheckConclusion != checksdb.CheckConclusionActionRequired ||
		checkRun.DetailsUrl != "/acme/demo/actions/runs/2" {
		t.Fatalf("quota-blocked check_run = %+v", checkRun)
	}
}

func TestEnqueue_AllowsOrgActionsWithMinutesOverride(t *testing.T) {
	f := setupOrgEnq(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	f.deps.Now = func() time.Time { return now }
	seedCompletedActionsMinutes(t, f, now, entitlements.FreeOrgActionsMinutesQuota)
	if _, err := billing.UpsertOrgQuotaOverride(ctx, billing.Deps{Pool: f.pool}, billing.QuotaOverrideInput{
		OrgID:           f.orgID,
		Kind:            billing.QuotaKindActionsMinutes,
		Unlimited:       true,
		CreatedByUserID: f.userID,
	}); err != nil {
		t.Fatalf("UpsertOrgQuotaOverride: %v", err)
	}

	res, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("a", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:quota-override",
		Workflow:       fixtureWorkflow(t),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if res.RunID == 0 || res.Skipped {
		t.Fatalf("expected enqueued run, got %+v", res)
	}
}

func seedCompletedActionsMinutes(t *testing.T, f enqFx, completedAt time.Time, minutes int64) {
	t.Helper()
	ctx := context.Background()
	res, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
		RepoID:         f.repoID,
		WorkflowFile:   ".shithub/workflows/seed.yml",
		HeadSHA:        strings.Repeat("f", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    f.userID,
		TriggerEventID: "push:quota-seed",
		Workflow:       fixtureWorkflow(t),
	})
	if err != nil {
		t.Fatalf("seed Enqueue: %v", err)
	}
	q := actionsdb.New()
	jobs, err := q.ListJobsForRun(ctx, f.pool, res.RunID)
	if err != nil {
		t.Fatalf("ListJobsForRun seed: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("seed jobs = %d, want 1", len(jobs))
	}
	runner, err := q.InsertRunner(ctx, f.pool, actionsdb.InsertRunnerParams{
		Name:               fmt.Sprintf("quota-seed-%d", f.repoID),
		Labels:             []string{"ubuntu-latest"},
		Capacity:           1,
		RegisteredByUserID: pgtype.Int8{Int64: f.userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertRunner seed: %v", err)
	}
	claimed, err := q.ClaimQueuedWorkflowJob(ctx, f.pool, actionsdb.ClaimQueuedWorkflowJobParams{
		Labels:   runner.Labels,
		RunnerID: runner.ID,
	})
	if err != nil {
		t.Fatalf("ClaimQueuedWorkflowJob seed: %v", err)
	}
	if claimed.ID != jobs[0].ID {
		t.Fatalf("claimed seed job id=%d, want %d", claimed.ID, jobs[0].ID)
	}
	startedAt := completedAt.Add(-time.Duration(minutes) * time.Minute)
	if _, err := f.pool.Exec(ctx, `UPDATE workflow_jobs SET timeout_minutes = $2 WHERE id = $1`, jobs[0].ID, minutes); err != nil {
		t.Fatalf("set seed timeout_minutes: %v", err)
	}
	if _, err := q.UpdateWorkflowJobStatus(ctx, f.pool, actionsdb.UpdateWorkflowJobStatusParams{
		ID:     jobs[0].ID,
		Status: actionsdb.WorkflowJobStatusCompleted,
		Conclusion: actionsdb.NullCheckConclusion{
			CheckConclusion: actionsdb.CheckConclusionSuccess,
			Valid:           true,
		},
		StartedAt:   pgtype.Timestamptz{Time: startedAt, Valid: true},
		CompletedAt: pgtype.Timestamptz{Time: completedAt, Valid: true},
	}); err != nil {
		t.Fatalf("UpdateWorkflowJobStatus seed: %v", err)
	}
	if _, err := q.CompleteWorkflowRun(ctx, f.pool, actionsdb.CompleteWorkflowRunParams{
		ID:         res.RunID,
		Conclusion: actionsdb.CheckConclusionSuccess,
	}); err != nil {
		t.Fatalf("CompleteWorkflowRun seed: %v", err)
	}
}

func TestListQueuedWorkflowJobRunsOnGroupsByRequestedLabel(t *testing.T) {
	f := setupEnq(t)
	ctx := context.Background()
	q := actionsdb.New()
	runner, err := q.InsertRunner(ctx, f.pool, actionsdb.InsertRunnerParams{
		Name:     "runner-linux",
		Labels:   []string{"self-hosted", "linux", "ubuntu-latest", "x64"},
		Capacity: 1,
	})
	if err != nil {
		t.Fatalf("InsertRunner: %v", err)
	}
	if _, err := q.HeartbeatRunner(ctx, f.pool, actionsdb.HeartbeatRunnerParams{
		ID:       runner.ID,
		Labels:   runner.Labels,
		Capacity: runner.Capacity,
		Status:   actionsdb.WorkflowRunnerStatusIdle,
	}); err != nil {
		t.Fatalf("HeartbeatRunner: %v", err)
	}

	for name, runsOn := range map[string]string{
		"linux":   "ubuntu-latest",
		"windows": "windows-latest",
	} {
		if _, err := trigger.Enqueue(ctx, f.deps, trigger.EnqueueParams{
			RepoID:         f.repoID,
			WorkflowFile:   ".shithub/workflows/" + name + ".yml",
			HeadSHA:        strings.Repeat(name[:1], 40),
			HeadRef:        "refs/heads/trunk",
			EventKind:      trigger.EventPush,
			EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
			ActorUserID:    f.userID,
			TriggerEventID: "push:queue-label-" + name,
			Workflow: workflowFromYAML(t, fmt.Sprintf(`name: %s
on: push
jobs:
  build:
    runs-on: %s
    steps:
      - run: echo hello
`, name, runsOn)),
		}); err != nil {
			t.Fatalf("Enqueue %s: %v", name, err)
		}
	}

	rows, err := q.ListQueuedWorkflowJobRunsOn(ctx, f.pool)
	if err != nil {
		t.Fatalf("ListQueuedWorkflowJobRunsOn: %v", err)
	}
	got := map[string]actionsdb.ListQueuedWorkflowJobRunsOnRow{}
	for _, row := range rows {
		got[row.RunsOn] = row
	}
	if got["ubuntu-latest"].QueuedJobs != 1 || got["ubuntu-latest"].MatchingRunnerCount != 1 {
		t.Fatalf("ubuntu-latest row: %+v", got["ubuntu-latest"])
	}
	if got["windows-latest"].QueuedJobs != 1 || got["windows-latest"].MatchingRunnerCount != 0 {
		t.Fatalf("windows-latest row: %+v", got["windows-latest"])
	}
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
