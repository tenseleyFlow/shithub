// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"
	"testing"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
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
