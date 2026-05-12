// SPDX-License-Identifier: AGPL-3.0-or-later

package cleanup

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const cleanupFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestSweepPrunesActionsRetentionSurfaces(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	store := storage.NewMemoryStore()
	now := time.Date(2026, 5, 12, 3, 30, 0, 0, time.UTC)

	repoID, userID := insertCleanupRepo(t, pool)
	q := actionsdb.New()

	chunkRun, oldChunkStep := insertCleanupRunJobStep(t, pool, repoID, userID, 1)
	setRunTerminal(t, pool, chunkRun.ID, now.Add(-90*24*time.Hour), true)
	setStepTerminal(t, pool, oldChunkStep.ID, now.Add(-8*24*time.Hour))
	appendChunk(t, pool, oldChunkStep.ID, 0, "old chunk")

	_, recentChunkStep := insertCleanupRunJobStep(t, pool, repoID, userID, 2)
	setStepTerminal(t, pool, recentChunkStep.ID, now.Add(-2*24*time.Hour))
	appendChunk(t, pool, recentChunkStep.ID, 0, "recent chunk")

	artifactRun, _ := insertCleanupRunJobStep(t, pool, repoID, userID, 3)
	artifactKey := "actions/runs/" + strconv.FormatInt(artifactRun.ID, 10) + "/artifacts/pkg.tgz"
	if _, err := store.Put(ctx, artifactKey, strings.NewReader("artifact"), storage.PutOpts{}); err != nil {
		t.Fatalf("store.Put artifact: %v", err)
	}
	artifact, err := q.InsertArtifact(ctx, pool, actionsdb.InsertArtifactParams{
		RunID:     artifactRun.ID,
		Name:      "pkg.tgz",
		ObjectKey: artifactKey,
		ByteCount: 8,
		ExpiresAt: pgtype.Timestamptz{
			Time:  now.Add(-24 * time.Hour),
			Valid: true,
		},
	})
	if err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}

	oldRun, _ := insertCleanupRunJobStep(t, pool, repoID, userID, 4)
	setRunTerminal(t, pool, oldRun.ID, now.Add(-366*24*time.Hour), false)
	pinnedRun, _ := insertCleanupRunJobStep(t, pool, repoID, userID, 5)
	setRunTerminal(t, pool, pinnedRun.ID, now.Add(-366*24*time.Hour), true)

	jwtRun, jwtJobStep := insertCleanupRunJobStep(t, pool, repoID, userID, 6)
	runner, err := q.InsertRunner(ctx, pool, actionsdb.InsertRunnerParams{
		Name:               "runner-retention",
		Labels:             []string{"ubuntu-latest"},
		Capacity:           1,
		RegisteredByUserID: pgtype.Int8{Int64: userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertRunner: %v", err)
	}
	if _, err := q.MarkRunnerJWTUsed(ctx, pool, actionsdb.MarkRunnerJWTUsedParams{
		Jti:       "old-jti-000000000",
		RunnerID:  runner.ID,
		JobID:     jwtJobStep.JobID,
		RunID:     jwtRun.ID,
		RepoID:    repoID,
		ExpiresAt: pgtype.Timestamptz{Time: now.Add(-31 * 24 * time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("MarkRunnerJWTUsed old: %v", err)
	}
	if _, err := q.MarkRunnerJWTUsed(ctx, pool, actionsdb.MarkRunnerJWTUsedParams{
		Jti:       "recent-jti-000000",
		RunnerID:  runner.ID,
		JobID:     jwtJobStep.JobID,
		RunID:     jwtRun.ID,
		RepoID:    repoID,
		ExpiresAt: pgtype.Timestamptz{Time: now.Add(-24 * time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("MarkRunnerJWTUsed recent: %v", err)
	}

	res, err := Sweep(ctx, Deps{Pool: pool, ObjectStore: store, Now: func() time.Time { return now }}, Payload{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ChunksDeleted != 1 || res.ArtifactRowsDeleted != 1 || res.ArtifactObjectsDeleted != 1 ||
		res.RunsDeleted != 1 || res.JWTUsedDeleted != 1 {
		t.Fatalf("unexpected cleanup result: %+v", res)
	}

	oldChunks, err := q.ListAllStepLogChunksForStep(ctx, pool, oldChunkStep.ID)
	if err != nil {
		t.Fatalf("ListAllStepLogChunksForStep old: %v", err)
	}
	if len(oldChunks) != 0 {
		t.Fatalf("old chunks survived: %+v", oldChunks)
	}
	recentChunks, err := q.ListAllStepLogChunksForStep(ctx, pool, recentChunkStep.ID)
	if err != nil {
		t.Fatalf("ListAllStepLogChunksForStep recent: %v", err)
	}
	if len(recentChunks) != 1 {
		t.Fatalf("recent chunks pruned: %+v", recentChunks)
	}
	if _, _, err := store.Get(ctx, artifactKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("artifact object: got %v, want not found", err)
	}
	if _, err := q.GetArtifactByID(ctx, pool, artifact.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("artifact row: got %v, want no rows", err)
	}
	if _, err := q.GetWorkflowRunByID(ctx, pool, oldRun.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old run: got %v, want no rows", err)
	}
	if _, err := q.GetWorkflowRunByID(ctx, pool, pinnedRun.ID); err != nil {
		t.Fatalf("pinned run pruned: %v", err)
	}
	assertJWTUsedExists(t, pool, "old-jti-000000000", false)
	assertJWTUsedExists(t, pool, "recent-jti-000000", true)
}

func insertCleanupRepo(t *testing.T, pool *pgxpool.Pool) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     "retention-alice",
		DisplayName:  "Retention Alice",
		PasswordHash: cleanupFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "retention-demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return repo.ID, user.ID
}

func insertCleanupRunJobStep(t *testing.T, pool *pgxpool.Pool, repoID, userID, runIndex int64) (actionsdb.WorkflowRun, actionsdb.WorkflowStep) {
	t.Helper()
	ctx := context.Background()
	q := actionsdb.New()
	run, err := q.InsertWorkflowRun(ctx, pool, actionsdb.InsertWorkflowRunParams{
		RepoID:       repoID,
		RunIndex:     runIndex,
		WorkflowFile: ".shithub/workflows/ci.yml",
		WorkflowName: "ci",
		HeadSha:      strings.Repeat("a", 40),
		HeadRef:      "refs/heads/trunk",
		Event:        actionsdb.WorkflowRunEventPush,
		EventPayload: []byte(`{}`),
		ActorUserID:  pgtype.Int8{Int64: userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertWorkflowRun: %v", err)
	}
	job, err := q.InsertWorkflowJob(ctx, pool, actionsdb.InsertWorkflowJobParams{
		RunID:          run.ID,
		JobIndex:       0,
		JobKey:         "build",
		JobName:        "build",
		RunsOn:         "ubuntu-latest",
		NeedsJobs:      []string{},
		TimeoutMinutes: 360,
		Permissions:    []byte(`{}`),
		JobEnv:         []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowJob: %v", err)
	}
	step, err := q.InsertWorkflowStep(ctx, pool, actionsdb.InsertWorkflowStepParams{
		JobID:            job.ID,
		StepIndex:        0,
		StepName:         "test",
		RunCommand:       "go test ./...",
		StepEnv:          []byte(`{}`),
		WorkingDirectory: "",
		StepWith:         []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowStep: %v", err)
	}
	return run, step
}

func setRunTerminal(t *testing.T, pool *pgxpool.Pool, runID int64, completedAt time.Time, pinned bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		UPDATE workflow_runs
		SET status = 'completed',
		    conclusion = 'success',
		    pinned = $2,
		    started_at = $3,
		    completed_at = $3,
		    updated_at = now()
		WHERE id = $1
	`, runID, pinned, completedAt)
	if err != nil {
		t.Fatalf("setRunTerminal: %v", err)
	}
}

func setStepTerminal(t *testing.T, pool *pgxpool.Pool, stepID int64, completedAt time.Time) {
	t.Helper()
	if _, err := actionsdb.New().UpdateWorkflowStepStatus(context.Background(), pool, actionsdb.UpdateWorkflowStepStatusParams{
		ID:          stepID,
		Status:      actionsdb.WorkflowStepStatusCompleted,
		Conclusion:  actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusionSuccess, Valid: true},
		StartedAt:   pgtype.Timestamptz{Time: completedAt.Add(-time.Minute), Valid: true},
		CompletedAt: pgtype.Timestamptz{Time: completedAt, Valid: true},
	}); err != nil {
		t.Fatalf("UpdateWorkflowStepStatus: %v", err)
	}
}

func appendChunk(t *testing.T, pool *pgxpool.Pool, stepID int64, seq int32, body string) {
	t.Helper()
	if _, err := actionsdb.New().AppendStepLogChunk(context.Background(), pool, actionsdb.AppendStepLogChunkParams{
		StepID: stepID,
		Seq:    seq,
		Chunk:  []byte(body),
	}); err != nil {
		t.Fatalf("AppendStepLogChunk: %v", err)
	}
}

func assertJWTUsedExists(t *testing.T, pool *pgxpool.Pool, jti string, want bool) {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM runner_jwt_used WHERE jti = $1)`, jti).Scan(&exists); err != nil {
		t.Fatalf("query runner_jwt_used %s: %v", jti, err)
	}
	if exists != want {
		t.Fatalf("runner_jwt_used %s exists=%t, want %t", jti, exists, want)
	}
}
