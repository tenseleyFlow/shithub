// SPDX-License-Identifier: AGPL-3.0-or-later

package finalize

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

const finalizeFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestHandlerUploadsConcatenatedLogAndDeletesChunks(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	store := storage.NewMemoryStore()
	_, job, step := insertFinalizeFixture(t, pool)

	q := actionsdb.New()
	if _, err := q.AppendStepLogChunk(ctx, pool, actionsdb.AppendStepLogChunkParams{
		StepID: step.ID,
		Seq:    0,
		Chunk:  []byte("hello "),
	}); err != nil {
		t.Fatalf("AppendStepLogChunk 0: %v", err)
	}
	if _, err := q.AppendStepLogChunk(ctx, pool, actionsdb.AppendStepLogChunkParams{
		StepID: step.ID,
		Seq:    1,
		Chunk:  []byte("world\n"),
	}); err != nil {
		t.Fatalf("AppendStepLogChunk 1: %v", err)
	}

	payload, err := json.Marshal(Payload{StepID: step.ID})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := Handler(Deps{Pool: pool, ObjectStore: store})(ctx, payload); err != nil {
		t.Fatalf("Handler: %v", err)
	}

	updated, err := q.GetWorkflowStepByID(ctx, pool, step.ID)
	if err != nil {
		t.Fatalf("GetWorkflowStepByID: %v", err)
	}
	wantKey := StepLogObjectKey(job.RunID, job.ID, step.ID)
	if !updated.LogObjectKey.Valid || updated.LogObjectKey.String != wantKey || updated.LogByteCount != int64(len("hello world\n")) {
		t.Fatalf("updated step log metadata: %+v", updated)
	}
	rc, meta, err := store.Get(ctx, wantKey)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if string(body) != "hello world\n" || meta.ContentType != logContentType {
		t.Fatalf("object: body=%q meta=%+v", body, meta)
	}
	chunks, err := q.ListStepLogChunks(ctx, pool, actionsdb.ListStepLogChunksParams{
		StepID: step.ID,
		Seq:    -1,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("ListStepLogChunks: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("chunks were not deleted: %+v", chunks)
	}
}

func TestHandlerPoisonsOversizedLogs(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	_, _, step := insertFinalizeFixture(t, pool)
	q := actionsdb.New()
	if _, err := q.AppendStepLogChunk(ctx, pool, actionsdb.AppendStepLogChunkParams{
		StepID: step.ID,
		Seq:    0,
		Chunk:  []byte("too large"),
	}); err != nil {
		t.Fatalf("AppendStepLogChunk: %v", err)
	}
	payload, err := json.Marshal(Payload{StepID: step.ID})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	err = Handler(Deps{Pool: pool, ObjectStore: storage.NewMemoryStore(), MaxLogBytes: 4})(ctx, payload)
	if !errors.Is(err, worker.ErrPoison) || !strings.Contains(err.Error(), "exceeds 4 bytes") {
		t.Fatalf("error: got %v, want poison oversized error", err)
	}
}

func insertFinalizeFixture(t *testing.T, pool *pgxpool.Pool) (actionsdb.WorkflowRun, actionsdb.WorkflowJob, actionsdb.WorkflowStep) {
	t.Helper()
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: finalizeFixtureHash,
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
	q := actionsdb.New()
	run, err := q.InsertWorkflowRun(ctx, pool, actionsdb.InsertWorkflowRunParams{
		RepoID:       repo.ID,
		RunIndex:     1,
		WorkflowFile: ".shithub/workflows/ci.yml",
		WorkflowName: "ci",
		HeadSha:      strings.Repeat("a", 40),
		HeadRef:      "refs/heads/trunk",
		Event:        actionsdb.WorkflowRunEventPush,
		EventPayload: []byte(`{}`),
		ActorUserID:  pgtype.Int8{Int64: user.ID, Valid: true},
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
	return run, job, step
}
