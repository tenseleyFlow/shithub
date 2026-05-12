// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos"
)

func TestActionsLifecycle_DisableThenEnableWorkflow(t *testing.T) {
	pool, router, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/ci.yml": ciYAML,
	})
	userID := ownerIDForAlice(t, pool)
	writeToken := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))
	readToken := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	// Disable.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/actions/workflows/ci.yml/disable", nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("disable status: got %d; body=%s", rr.Code, rr.Body.String())
	}

	// List should show state=disabled.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/workflows", nil)
	req.Header.Set("Authorization", "Bearer "+readToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var listed []apiWorkflow
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) == 0 || listed[0].State != "disabled" {
		t.Errorf("list after disable: got %+v", listed)
	}

	// Enable.
	req = httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/actions/workflows/ci.yml/enable", nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("enable status: got %d; body=%s", rr.Code, rr.Body.String())
	}

	// List should be active again.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/workflows", nil)
	req.Header.Set("Authorization", "Bearer "+readToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) == 0 || listed[0].State != "active" {
		t.Errorf("list after enable: got %+v", listed)
	}
}

func TestActionsLifecycle_DisableRequiresRepoWrite(t *testing.T) {
	_, router, _, token, _, _ := seedBranchesEnv(t, "alice")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/actions/workflows/ci.yml/disable", nil)
	req.Header.Set("Authorization", "Bearer "+token) // repo:read only
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsLifecycle_RunDelete(t *testing.T) {
	pool, router, userID, repoID, _ := seedIssuesEnv(t, "alice")
	run := seedWorkflowRun(t, pool, repoID, userID, 1, ".shithub/workflows/ci.yml")
	writeToken := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/actions/runs/"+strconv.FormatInt(run.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	if _, err := actionsdb.New().GetWorkflowRunByID(context.Background(), pool, run.ID); err == nil {
		t.Errorf("run still present after delete")
	}
}

func TestActionsLifecycle_RunDeleteUnknown404(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	writeToken := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/actions/runs/99999", nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsLifecycle_ArtifactsListAndGet(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	run := seedWorkflowRun(t, pool, repoID, userID, 1, ".shithub/workflows/ci.yml")
	art, err := actionsdb.New().InsertArtifact(context.Background(), pool, actionsdb.InsertArtifactParams{
		RunID:     run.ID,
		Name:      "logs",
		ObjectKey: "actions/runs/1/logs.zip",
		ByteCount: 42,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(48 * time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/runs/"+strconv.FormatInt(run.ID, 10)+"/artifacts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Fatalf("expected 1 artifact; got %+v", listed)
	}
	if name, _ := listed[0]["name"].(string); name != "logs" {
		t.Errorf("name: %+v", listed[0])
	}
	if archive, _ := listed[0]["archive_url"].(string); !strings.Contains(archive, "/actions/artifacts/") {
		t.Errorf("archive_url: %+v", listed[0])
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/artifacts/"+strconv.FormatInt(art.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status: got %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsLifecycle_ArtifactCrossRepo404(t *testing.T) {
	pool, router, userID, repoID, _ := seedIssuesEnv(t, "alice")
	run := seedWorkflowRun(t, pool, repoID, userID, 1, ".shithub/workflows/ci.yml")
	art, err := actionsdb.New().InsertArtifact(context.Background(), pool, actionsdb.InsertArtifactParams{
		RunID:     run.ID,
		Name:      "logs",
		ObjectKey: "actions/runs/1/logs.zip",
		ByteCount: 42,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(48 * time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}

	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead))
	// Create a separate fork repo owned by bob using the repos orchestrator.
	rfs, rerr := storage.NewRepoFS(t.TempDir())
	if rerr != nil {
		t.Fatalf("NewRepoFS: %v", rerr)
	}
	if _, err := repos.Create(context.Background(), repos.Deps{
		Pool:    pool,
		RepoFS:  rfs,
		Audit:   audit.NewRecorder(),
		Limiter: throttle.NewLimiter(),
	}, repos.Params{
		ActorUserID:   bobID,
		OwnerUserID:   bobID,
		OwnerUsername: "bob",
		Name:          "fork",
		Description:   "bob fork",
		Visibility:    "public",
	}); err != nil {
		t.Fatalf("repos.Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/bob/fork/actions/artifacts/"+strconv.FormatInt(art.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsLifecycle_JobLogsAssemble(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	run := seedWorkflowRun(t, pool, repoID, userID, 1, ".shithub/workflows/ci.yml")
	q := actionsdb.New()
	job, err := q.InsertWorkflowJob(context.Background(), pool, actionsdb.InsertWorkflowJobParams{
		RunID:          run.ID,
		JobIndex:       0,
		JobKey:         "build",
		JobName:        "Build",
		RunsOn:         "ubuntu-latest",
		NeedsJobs:      []string{},
		TimeoutMinutes: 60,
		Permissions:    []byte(`{}`),
		JobEnv:         []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowJob: %v", err)
	}
	step, err := q.InsertWorkflowStep(context.Background(), pool, actionsdb.InsertWorkflowStepParams{
		JobID:      job.ID,
		StepIndex:  0,
		StepID:     "checkout",
		StepName:   "Check out",
		RunCommand: "",
		UsesAlias:  "actions/checkout@v4",
		StepEnv:    []byte(`{}`),
		StepWith:   []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowStep: %v", err)
	}
	if _, err := q.AppendStepLogChunk(context.Background(), pool, actionsdb.AppendStepLogChunkParams{
		StepID: step.ID, Seq: 1, Chunk: []byte("hello\nworld\n"),
	}); err != nil {
		t.Fatalf("AppendStepLogChunk: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/jobs/"+strconv.FormatInt(job.ID, 10)+"/logs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "hello\nworld") {
		t.Errorf("log body missing payload: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "step 0: Check out") {
		t.Errorf("step header missing: %s", rr.Body.String())
	}
}
