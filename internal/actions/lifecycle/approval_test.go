// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"
	"testing"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

func TestApproveRunDoesNotRecordApprovalForNonQueuedRun(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	repoID, userID := setupLifecycleRepo(t, pool)
	q := actionsdb.New()
	run := insertLifecycleRun(t, pool, repoID, userID, 1)
	if _, err := pool.Exec(ctx, `
		UPDATE workflow_runs
		SET need_approval = true,
		    status = 'completed',
		    conclusion = 'success',
		    started_at = now(),
		    completed_at = now()
		WHERE id = $1`,
		run.ID,
	); err != nil {
		t.Fatalf("mark run completed: %v", err)
	}
	if _, err := q.InsertWorkflowRunApproval(ctx, pool, actionsdb.InsertWorkflowRunApprovalParams{
		RunID:           run.ID,
		RequestedReason: "approval required",
	}); err != nil {
		t.Fatalf("InsertWorkflowRunApproval: %v", err)
	}

	_, err := ApproveRun(ctx, Deps{Pool: pool}, run.ID, userID)
	if !errors.Is(err, ErrRunNotApprovalPending) {
		t.Fatalf("ApproveRun error = %v, want ErrRunNotApprovalPending", err)
	}
	approval, err := q.GetWorkflowRunApproval(ctx, pool, run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRunApproval: %v", err)
	}
	if approval.ApprovedByUserID.Valid || approval.ApprovedAt.Valid {
		t.Fatalf("approval row changed for non-queued run: %+v", approval)
	}
	gotRun, err := q.GetWorkflowRunByID(ctx, pool, run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if gotRun.ApprovedByUserID.Valid {
		t.Fatalf("run was approved despite terminal state: %+v", gotRun)
	}
}

func TestApproveRunBlocksSelfReviewForEnvironment(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	repoID, userID := setupLifecycleRepo(t, pool)
	q := actionsdb.New()
	run := insertLifecycleRun(t, pool, repoID, userID, 1)
	if _, err := pool.Exec(ctx, `
		UPDATE workflow_runs
		SET need_approval = true
		WHERE id = $1`,
		run.ID,
	); err != nil {
		t.Fatalf("mark run approval-pending: %v", err)
	}
	if _, err := q.InsertWorkflowRunApproval(ctx, pool, actionsdb.InsertWorkflowRunApprovalParams{
		RunID:           run.ID,
		RequestedReason: "Deployment to production requires environment approval before runner dispatch.",
	}); err != nil {
		t.Fatalf("InsertWorkflowRunApproval: %v", err)
	}
	if _, err := q.UpsertRepoEnvironment(ctx, pool, actionsdb.UpsertRepoEnvironmentParams{
		RepoID:                   repoID,
		Name:                     "production",
		RequiredReviewersEnabled: true,
		PreventSelfReview:        true,
		WaitTimerMinutes:         0,
		DeploymentBranchPolicy:   actionsdb.RepoEnvironmentDeploymentBranchPolicyAll,
	}); err != nil {
		t.Fatalf("UpsertRepoEnvironment: %v", err)
	}
	if _, err := q.InsertWorkflowJob(ctx, pool, actionsdb.InsertWorkflowJobParams{
		RunID:           run.ID,
		JobIndex:        1,
		JobKey:          "deploy",
		JobName:         "deploy",
		RunsOn:          "ubuntu-latest",
		NeedsJobs:       []string{},
		TimeoutMinutes:  30,
		Permissions:     []byte(`{}`),
		JobEnv:          []byte(`{}`),
		EnvironmentName: "production",
	}); err != nil {
		t.Fatalf("InsertWorkflowJob: %v", err)
	}

	_, err := ApproveRun(ctx, Deps{Pool: pool}, run.ID, userID)
	if !errors.Is(err, ErrApprovalSelfReviewBlocked) {
		t.Fatalf("ApproveRun self-review error = %v, want ErrApprovalSelfReviewBlocked", err)
	}
	approval, err := q.GetWorkflowRunApproval(ctx, pool, run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRunApproval: %v", err)
	}
	if approval.ApprovedByUserID.Valid {
		t.Fatalf("approval was recorded despite self-review guard: %+v", approval)
	}

	reviewer, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "bob", DisplayName: "Bob", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser reviewer: %v", err)
	}
	if _, err := ApproveRun(ctx, Deps{Pool: pool}, run.ID, reviewer.ID); err != nil {
		t.Fatalf("ApproveRun reviewer: %v", err)
	}
}
