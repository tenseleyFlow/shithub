// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

const oldWorkflow = `name: CI
on: push
jobs:
  old_job:
    name: Old job
    runs-on: ubuntu-latest
    steps:
      - run: echo old
`

const newWorkflow = `name: CI
on: push
jobs:
  new_job:
    name: New job
    runs-on: ubuntu-latest
    steps:
      - run: echo new
`

func TestRerunRunUsesOriginalCommitWorkflowAndParentsNewRun(t *testing.T) {
	ctx := context.Background()
	pool := dbtestPool(t)
	repoID, userID := setupLifecycleRepo(t, pool)
	rfs := lifecycleRepoFS(t)
	gitDir := lifecycleGitDir(t, rfs)
	oldSHA := lifecycleCommitWorkflow(t, gitDir, oldWorkflow, time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC))
	wf := parseLifecycleWorkflow(t, oldWorkflow)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	original, err := trigger.Enqueue(ctx, trigger.Deps{Pool: pool, Logger: logger}, trigger.EnqueueParams{
		RepoID:         repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        oldSHA,
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    userID,
		TriggerEventID: "push:original",
		Workflow:       wf,
	})
	if err != nil {
		t.Fatalf("trigger.Enqueue original: %v", err)
	}
	if _, err := actionsdb.New().CompleteWorkflowRun(ctx, pool, actionsdb.CompleteWorkflowRunParams{
		ID:         original.RunID,
		Conclusion: actionsdb.CheckConclusionFailure,
	}); err != nil {
		t.Fatalf("CompleteWorkflowRun: %v", err)
	}
	_ = lifecycleCommitWorkflow(t, gitDir, newWorkflow, time.Date(2026, 5, 11, 12, 5, 0, 0, time.UTC))

	result, err := RerunRun(ctx, Deps{Pool: pool, RepoFS: rfs, Logger: logger}, original.RunID, userID)
	if err != nil {
		t.Fatalf("RerunRun: %v", err)
	}
	if result.ParentRunID != original.RunID || result.RunID == 0 || result.RunID == original.RunID {
		t.Fatalf("unexpected result: %+v", result)
	}

	q := actionsdb.New()
	rerun, err := q.GetWorkflowRunByID(ctx, pool, result.RunID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID rerun: %v", err)
	}
	if !rerun.ParentRunID.Valid || rerun.ParentRunID.Int64 != original.RunID {
		t.Fatalf("parent_run_id: %+v want %d", rerun.ParentRunID, original.RunID)
	}
	if rerun.HeadSha != oldSHA {
		t.Fatalf("rerun head_sha = %q want original %q", rerun.HeadSha, oldSHA)
	}
	if !strings.HasPrefix(rerun.TriggerEventID, "rerun:") {
		t.Fatalf("rerun trigger_event_id = %q", rerun.TriggerEventID)
	}
	if !rerun.ActorUserID.Valid || rerun.ActorUserID.Int64 != userID {
		t.Fatalf("rerun actor_user_id: %+v want %d", rerun.ActorUserID, userID)
	}
	jobs, err := q.ListJobsForRun(ctx, pool, result.RunID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 || jobs[0].JobKey != "old_job" || jobs[0].JobName != "Old job" {
		t.Fatalf("rerun used wrong workflow jobs: %+v", jobs)
	}
}

func TestRerunRunRejectsNonTerminalRun(t *testing.T) {
	ctx := context.Background()
	pool := dbtestPool(t)
	repoID, userID := setupLifecycleRepo(t, pool)
	rfs := lifecycleRepoFS(t)
	run := insertLifecycleRun(t, pool, repoID, userID, 1)

	_, err := RerunRun(ctx, Deps{Pool: pool, RepoFS: rfs}, run.ID, userID)
	if !errors.Is(err, ErrRunNotRerunnable) {
		t.Fatalf("RerunRun error = %v, want ErrRunNotRerunnable", err)
	}
}

func dbtestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.NewTestDB(t)
}

func lifecycleRepoFS(t *testing.T) *storage.RepoFS {
	t.Helper()
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	return rfs
}

func lifecycleGitDir(t *testing.T, rfs *storage.RepoFS) string {
	t.Helper()
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := rfs.InitBare(context.Background(), gitDir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	return gitDir
}

func lifecycleCommitWorkflow(t *testing.T, gitDir, body string, when time.Time) string {
	t.Helper()
	commit, err := (repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.test",
		Branch:      "trunk",
		Message:     "Update workflow",
		When:        when,
		Files: []repogit.FileEntry{
			{Path: ".shithub/workflows/ci.yml", Body: []byte(body)},
		},
	}).Build(context.Background())
	if err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	return commit
}

func parseLifecycleWorkflow(t *testing.T, body string) *workflow.Workflow {
	t.Helper()
	wf, diags, err := workflow.Parse([]byte(body))
	if err != nil {
		t.Fatalf("workflow.Parse: %v", err)
	}
	for _, d := range diags {
		if d.Severity == workflow.Error {
			t.Fatalf("workflow diagnostic: %v", d)
		}
	}
	return wf
}
