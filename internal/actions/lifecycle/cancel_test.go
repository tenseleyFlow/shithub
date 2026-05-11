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

func setupLifecycleRepo(t *testing.T, db actionsdb.DBTX) (repoID, userID int64) {
	t.Helper()
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, db, usersdb.CreateUserParams{
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, db, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return repo.ID, user.ID
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
