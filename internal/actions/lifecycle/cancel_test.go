// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestCancelRunCancelsQueuedJobsAndCompletesRun(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	repoID, userID := setupLifecycleRepo(t, pool)
	q := actionsdb.New()
	run := insertLifecycleRun(t, pool, repoID, userID, 1)
	job, err := q.InsertWorkflowJob(ctx, pool, actionsdb.InsertWorkflowJobParams{
		RunID:          run.ID,
		JobIndex:       0,
		JobKey:         "build",
		JobName:        "Build",
		RunsOn:         "ubuntu-latest",
		NeedsJobs:      []string{},
		TimeoutMinutes: 30,
		Permissions:    []byte(`{}`),
		JobEnv:         []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowJob: %v", err)
	}
	step, err := q.InsertWorkflowStep(ctx, pool, actionsdb.InsertWorkflowStepParams{
		JobID:      job.ID,
		StepIndex:  0,
		RunCommand: "go test ./...",
		StepEnv:    []byte(`{}`),
		StepWith:   []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowStep: %v", err)
	}

	result, err := CancelRun(ctx, Deps{Pool: pool}, run.ID, CancelReasonUser)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if len(result.ChangedJobs) != 1 || !result.RunCompleted || result.RunConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("result: %+v", result)
	}
	gotJob, err := q.GetWorkflowJobByID(ctx, pool, job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID: %v", err)
	}
	if gotJob.Status != actionsdb.WorkflowJobStatusCancelled || !gotJob.CancelRequested ||
		!gotJob.Conclusion.Valid || gotJob.Conclusion.CheckConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("job: %+v", gotJob)
	}
	gotStep, err := q.GetWorkflowStepByID(ctx, pool, step.ID)
	if err != nil {
		t.Fatalf("GetWorkflowStepByID: %v", err)
	}
	if gotStep.Status != actionsdb.WorkflowStepStatusCancelled ||
		!gotStep.Conclusion.Valid || gotStep.Conclusion.CheckConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("step: %+v", gotStep)
	}
	gotRun, err := q.GetWorkflowRunByID(ctx, pool, run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if gotRun.Status != actionsdb.WorkflowRunStatusCompleted ||
		!gotRun.Conclusion.Valid || gotRun.Conclusion.CheckConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("run: %+v", gotRun)
	}
}

func TestCancelJobRequestsRunningJobWithoutTerminalOverwrite(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	repoID, userID := setupLifecycleRepo(t, pool)
	q := actionsdb.New()
	run := insertLifecycleRun(t, pool, repoID, userID, 2)
	job, err := q.InsertWorkflowJob(ctx, pool, actionsdb.InsertWorkflowJobParams{
		RunID:          run.ID,
		JobIndex:       0,
		JobKey:         "test",
		JobName:        "Test",
		RunsOn:         "ubuntu-latest",
		NeedsJobs:      []string{},
		TimeoutMinutes: 30,
		Permissions:    []byte(`{}`),
		JobEnv:         []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowJob: %v", err)
	}
	runner, err := q.InsertRunner(ctx, pool, actionsdb.InsertRunnerParams{
		Name:     "runner-1",
		Labels:   []string{"ubuntu-latest"},
		Capacity: 1,
	})
	if err != nil {
		t.Fatalf("InsertRunner: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_jobs SET runner_id = $1, status = 'running', started_at = now() WHERE id = $2`, runner.ID, job.ID); err != nil {
		t.Fatalf("mark job running: %v", err)
	}

	result, err := CancelJob(ctx, Deps{Pool: pool}, job.ID, CancelReasonUser)
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if len(result.ChangedJobs) != 1 || result.RunCompleted {
		t.Fatalf("result: %+v", result)
	}
	gotJob, err := q.GetWorkflowJobByID(ctx, pool, job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID: %v", err)
	}
	if gotJob.Status != actionsdb.WorkflowJobStatusRunning || !gotJob.CancelRequested || gotJob.Conclusion.Valid {
		t.Fatalf("job: %+v", gotJob)
	}
	gotRun, err := q.GetWorkflowRunByID(ctx, pool, run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if gotRun.Status != actionsdb.WorkflowRunStatusRunning || gotRun.Conclusion.Valid {
		t.Fatalf("run: %+v", gotRun)
	}

	again, err := CancelJob(ctx, Deps{Pool: pool}, job.ID, CancelReasonUser)
	if err != nil {
		t.Fatalf("CancelJob repeat: %v", err)
	}
	if len(again.ChangedJobs) != 0 {
		t.Fatalf("repeat was not idempotent: %+v", again)
	}
}

func TestReconcileRunnerActiveJobsCancelsOnlyMissingRunningJobs(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	repoID, userID := setupLifecycleRepo(t, pool)
	q := actionsdb.New()
	run := insertLifecycleRun(t, pool, repoID, userID, 3)
	runner, err := q.InsertRunner(ctx, pool, actionsdb.InsertRunnerParams{
		Name:     "runner-reconcile",
		Labels:   []string{"ubuntu-latest"},
		Capacity: 2,
	})
	if err != nil {
		t.Fatalf("InsertRunner: %v", err)
	}
	orphan := insertLifecycleJob(t, pool, run.ID, 0, "orphan")
	active := insertLifecycleJob(t, pool, run.ID, 1, "active")
	orphanStep := insertLifecycleStep(t, pool, orphan.ID, 0)
	activeStep := insertLifecycleStep(t, pool, active.ID, 0)
	if _, err := pool.Exec(ctx, `
		UPDATE workflow_jobs
		   SET runner_id = $1, status = 'running', started_at = now()
		 WHERE id = ANY($2::bigint[])`,
		runner.ID, []int64{orphan.ID, active.ID}); err != nil {
		t.Fatalf("mark jobs running: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workflow_steps
		   SET status = 'running', started_at = now()
		 WHERE id = ANY($1::bigint[])`,
		[]int64{orphanStep.ID, activeStep.ID}); err != nil {
		t.Fatalf("mark steps running: %v", err)
	}

	result, err := ReconcileRunnerActiveJobs(ctx, Deps{Pool: pool}, runner.ID, []int64{active.ID})
	if err != nil {
		t.Fatalf("ReconcileRunnerActiveJobs: %v", err)
	}
	if len(result.ChangedJobs) != 1 || result.ChangedJobs[0].ID != orphan.ID || len(result.CompletedRuns) != 0 {
		t.Fatalf("result: %+v", result)
	}
	gotOrphan, err := q.GetWorkflowJobByID(ctx, pool, orphan.ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID orphan: %v", err)
	}
	if gotOrphan.Status != actionsdb.WorkflowJobStatusCancelled || !gotOrphan.CancelRequested ||
		!gotOrphan.Conclusion.Valid || gotOrphan.Conclusion.CheckConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("orphan job: %+v", gotOrphan)
	}
	gotOrphanStep, err := q.GetWorkflowStepByID(ctx, pool, orphanStep.ID)
	if err != nil {
		t.Fatalf("GetWorkflowStepByID orphan: %v", err)
	}
	if gotOrphanStep.Status != actionsdb.WorkflowStepStatusCancelled ||
		!gotOrphanStep.Conclusion.Valid || gotOrphanStep.Conclusion.CheckConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("orphan step: %+v", gotOrphanStep)
	}
	gotActive, err := q.GetWorkflowJobByID(ctx, pool, active.ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID active: %v", err)
	}
	if gotActive.Status != actionsdb.WorkflowJobStatusRunning || gotActive.CancelRequested || gotActive.Conclusion.Valid {
		t.Fatalf("active job changed: %+v", gotActive)
	}
	gotActiveStep, err := q.GetWorkflowStepByID(ctx, pool, activeStep.ID)
	if err != nil {
		t.Fatalf("GetWorkflowStepByID active: %v", err)
	}
	if gotActiveStep.Status != actionsdb.WorkflowStepStatusRunning || gotActiveStep.Conclusion.Valid {
		t.Fatalf("active step changed: %+v", gotActiveStep)
	}
	gotRun, err := q.GetWorkflowRunByID(ctx, pool, run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if gotRun.Status != actionsdb.WorkflowRunStatusRunning || gotRun.Conclusion.Valid {
		t.Fatalf("run after partial reconcile: %+v", gotRun)
	}

	again, err := ReconcileRunnerActiveJobs(ctx, Deps{Pool: pool}, runner.ID, []int64{active.ID})
	if err != nil {
		t.Fatalf("ReconcileRunnerActiveJobs repeat: %v", err)
	}
	if len(again.ChangedJobs) != 0 {
		t.Fatalf("repeat changed jobs: %+v", again)
	}

	final, err := ReconcileRunnerActiveJobs(ctx, Deps{Pool: pool}, runner.ID, []int64{})
	if err != nil {
		t.Fatalf("ReconcileRunnerActiveJobs final: %v", err)
	}
	if len(final.ChangedJobs) != 1 || final.ChangedJobs[0].ID != active.ID || len(final.CompletedRuns) != 1 {
		t.Fatalf("final result: %+v", final)
	}
	gotRun, err = q.GetWorkflowRunByID(ctx, pool, run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID final: %v", err)
	}
	if gotRun.Status != actionsdb.WorkflowRunStatusCompleted ||
		!gotRun.Conclusion.Valid || gotRun.Conclusion.CheckConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("run after final reconcile: %+v", gotRun)
	}
}

func TestListActiveWorkflowRunsForAdminFiltersActiveRuns(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	repoID, userID := setupLifecycleRepo(t, pool)
	q := actionsdb.New()

	queued := insertLifecycleRun(t, pool, repoID, userID, 1)
	running := insertLifecycleRun(t, pool, repoID, userID, 2)
	running, err := q.StartWorkflowRun(ctx, pool, running.ID)
	if err != nil {
		t.Fatalf("StartWorkflowRun: %v", err)
	}
	completed := insertLifecycleRun(t, pool, repoID, userID, 3)
	if _, err := q.CompleteWorkflowRun(ctx, pool, actionsdb.CompleteWorkflowRunParams{
		ID:         completed.ID,
		Conclusion: actionsdb.CheckConclusionSuccess,
	}); err != nil {
		t.Fatalf("CompleteWorkflowRun: %v", err)
	}
	otherRepoID, otherUserID := setupNamedLifecycleRepo(t, pool, "bob", "other")
	otherRepoRun := insertLifecycleRun(t, pool, otherRepoID, otherUserID, 1)

	all, err := q.ListActiveWorkflowRunsForAdmin(ctx, pool, actionsdb.ListActiveWorkflowRunsForAdminParams{
		RepoID:     0,
		LimitCount: 10,
	})
	if err != nil {
		t.Fatalf("ListActiveWorkflowRunsForAdmin all: %v", err)
	}
	assertRunIDs(t, all, queued.ID, running.ID, otherRepoRun.ID)

	repoOnly, err := q.ListActiveWorkflowRunsForAdmin(ctx, pool, actionsdb.ListActiveWorkflowRunsForAdminParams{
		RepoID:     repoID,
		LimitCount: 10,
	})
	if err != nil {
		t.Fatalf("ListActiveWorkflowRunsForAdmin repo: %v", err)
	}
	assertRunIDs(t, repoOnly, queued.ID, running.ID)

	limited, err := q.ListActiveWorkflowRunsForAdmin(ctx, pool, actionsdb.ListActiveWorkflowRunsForAdminParams{
		RepoID:     0,
		LimitCount: 1,
	})
	if err != nil {
		t.Fatalf("ListActiveWorkflowRunsForAdmin limited: %v", err)
	}
	assertRunIDs(t, limited, queued.ID)
}

func insertLifecycleJob(t *testing.T, db actionsdb.DBTX, runID int64, index int32, key string) actionsdb.WorkflowJob {
	t.Helper()
	job, err := actionsdb.New().InsertWorkflowJob(context.Background(), db, actionsdb.InsertWorkflowJobParams{
		RunID:          runID,
		JobIndex:       index,
		JobKey:         key,
		JobName:        key,
		RunsOn:         "ubuntu-latest",
		NeedsJobs:      []string{},
		TimeoutMinutes: 30,
		Permissions:    []byte(`{}`),
		JobEnv:         []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowJob %s: %v", key, err)
	}
	return job
}

func insertLifecycleStep(t *testing.T, db actionsdb.DBTX, jobID int64, index int32) actionsdb.WorkflowStep {
	t.Helper()
	step, err := actionsdb.New().InsertWorkflowStep(context.Background(), db, actionsdb.InsertWorkflowStepParams{
		JobID:      jobID,
		StepIndex:  index,
		RunCommand: "true",
		StepEnv:    []byte(`{}`),
		StepWith:   []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowStep: %v", err)
	}
	return step
}

func setupLifecycleRepo(t *testing.T, db actionsdb.DBTX) (repoID, userID int64) {
	t.Helper()
	return setupNamedLifecycleRepo(t, db, "alice", "demo")
}

func setupNamedLifecycleRepo(t *testing.T, db actionsdb.DBTX, username, repoName string) (repoID, userID int64) {
	t.Helper()
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, db, usersdb.CreateUserParams{
		Username:     username,
		DisplayName:  username,
		PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, db, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          repoName,
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return repo.ID, user.ID
}

func assertRunIDs(t *testing.T, runs []actionsdb.WorkflowRun, want ...int64) {
	t.Helper()
	if len(runs) != len(want) {
		t.Fatalf("got %d runs, want %d: %+v", len(runs), len(want), runs)
	}
	for i := range want {
		if runs[i].ID != want[i] {
			t.Fatalf("run[%d] id=%d, want %d; runs=%+v", i, runs[i].ID, want[i], runs)
		}
	}
}

func insertLifecycleRun(t *testing.T, db actionsdb.DBTX, repoID, userID, runIndex int64) actionsdb.WorkflowRun {
	t.Helper()
	run, err := actionsdb.New().InsertWorkflowRun(context.Background(), db, actionsdb.InsertWorkflowRunParams{
		RepoID:       repoID,
		RunIndex:     runIndex,
		WorkflowFile: ".shithub/workflows/ci.yml",
		WorkflowName: "CI",
		HeadSha:      strings.Repeat("a", 40),
		HeadRef:      "trunk",
		Event:        actionsdb.WorkflowRunEventPush,
		EventPayload: []byte(`{}`),
		ActorUserID:  pgtype.Int8{Int64: userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertWorkflowRun: %v", err)
	}
	return run
}
