// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/actions/runnertoken"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
)

const runnerAPIFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestRunnerHeartbeatClaimsQueuedJob(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	runID := enqueueRunnerAPIRun(t, pool, logger, repoID, userID)

	token, runnerID := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 1)
	signer := runnerAPISigner(t, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	router := newRunnerAPIRouter(t, pool, logger, signer)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
		Job   struct {
			ID     int64 `json:"id"`
			RunID  int64 `json:"run_id"`
			RepoID int64 `json:"repo_id"`
			Steps  []struct {
				Run  string `json:"run"`
				Uses string `json:"uses"`
			} `json:"steps"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("response token is empty")
	}
	if resp.Job.RunID != runID || resp.Job.RepoID != repoID || len(resp.Job.Steps) != 2 {
		t.Fatalf("unexpected job payload: %+v", resp.Job)
	}
	claims, err := signer.Verify(resp.Token)
	if err != nil {
		t.Fatalf("verify runner JWT: %v", err)
	}
	if claims.JobID != resp.Job.ID || claims.RunID != runID || claims.RepoID != repoID {
		t.Fatalf("claims/job mismatch: claims=%+v job=%+v", claims, resp.Job)
	}
	claimRunnerID, err := claims.RunnerID()
	if err != nil {
		t.Fatalf("claims RunnerID: %v", err)
	}
	if claimRunnerID != runnerID {
		t.Fatalf("claims runner_id: got %d, want %d", claimRunnerID, runnerID)
	}

	q := actionsdb.New()
	job, err := q.GetWorkflowJobByID(ctx, pool, resp.Job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID: %v", err)
	}
	if job.Status != actionsdb.WorkflowJobStatusRunning || !job.RunnerID.Valid || job.RunnerID.Int64 != runnerID {
		t.Fatalf("job not claimed by runner: %+v", job)
	}
	run, err := q.GetWorkflowRunByID(ctx, pool, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if run.Status != actionsdb.WorkflowRunStatusRunning {
		t.Fatalf("run status: got %s, want running", run.Status)
	}

	// Capacity is enforced server-side: a second heartbeat from the same
	// runner sees one running job and receives no additional claim.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("second heartbeat status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunnerHeartbeatRejectsBadToken(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newRunnerAPIRouter(t, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), runnerAPISigner(t, time.Now()))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer not-hex")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func newRunnerAPIRouter(t *testing.T, pool *pgxpool.Pool, logger *slog.Logger, signer *runnerjwt.Signer) http.Handler {
	t.Helper()
	h, err := apih.New(apih.Deps{
		Pool:      pool,
		Logger:    logger,
		RunnerJWT: signer,
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

func setupRunnerAPIRepo(t *testing.T, pool *pgxpool.Pool) (repoID, userID int64) {
	t.Helper()
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: runnerAPIFixtureHash,
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
	return repo.ID, user.ID
}

func enqueueRunnerAPIRun(t *testing.T, pool *pgxpool.Pool, logger *slog.Logger, repoID, userID int64) int64 {
	t.Helper()
	wf, diags, err := workflow.Parse([]byte(`name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
`))
	if err != nil {
		t.Fatalf("workflow.Parse: %v", err)
	}
	for _, d := range diags {
		if d.Severity == workflow.Error {
			t.Fatalf("workflow diagnostic: %v", d)
		}
	}
	res, err := trigger.Enqueue(context.Background(), trigger.Deps{Pool: pool, Logger: logger}, trigger.EnqueueParams{
		RepoID:         repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("a", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    userID,
		TriggerEventID: "push:test",
		Workflow:       wf,
	})
	if err != nil {
		t.Fatalf("trigger.Enqueue: %v", err)
	}
	return res.RunID
}

func registerRunnerForTest(t *testing.T, pool *pgxpool.Pool, labels []string, capacity int32) (token string, runnerID int64) {
	t.Helper()
	token, tokenHash, err := runnertoken.New()
	if err != nil {
		t.Fatalf("runnertoken.New: %v", err)
	}
	q := actionsdb.New()
	runner, err := q.InsertRunner(context.Background(), pool, actionsdb.InsertRunnerParams{
		Name:     "runner1",
		Labels:   labels,
		Capacity: capacity,
	})
	if err != nil {
		t.Fatalf("InsertRunner: %v", err)
	}
	if _, err := q.InsertRunnerToken(context.Background(), pool, actionsdb.InsertRunnerTokenParams{
		RunnerID:  runner.ID,
		TokenHash: tokenHash,
	}); err != nil {
		t.Fatalf("InsertRunnerToken: %v", err)
	}
	return token, runner.ID
}

func runnerAPISigner(t *testing.T, now time.Time) *runnerjwt.Signer {
	t.Helper()
	signer, err := runnerjwt.NewFromKey(
		bytes.Repeat([]byte{0x7c}, 32),
		runnerjwt.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("runnerjwt.NewFromKey: %v", err)
	}
	return signer
}
