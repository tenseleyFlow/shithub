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

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
)

type apiActionsRun struct {
	ID            int64  `json:"id"`
	RunNumber     int64  `json:"run_number"`
	WorkflowFile  string `json:"workflow_file"`
	WorkflowName  string `json:"workflow_name"`
	HeadSHA       string `json:"head_sha"`
	HeadRef       string `json:"head_ref"`
	Event         string `json:"event"`
	Status        string `json:"status"`
	Conclusion    string `json:"conclusion"`
	ActorUsername string `json:"actor_username"`
	CreatedAt     string `json:"created_at"`
}

type apiActionsJob struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	JobIndex   int32  `json:"job_index"`
	JobKey     string `json:"job_key"`
	JobName    string `json:"job_name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// seedWorkflowRun inserts a workflow_runs row for the given repo and
// returns the row. Each call uses an incrementing run_index so
// successive seeds in the same test never collide.
func seedWorkflowRun(t *testing.T, pool *pgxpool.Pool, repoID, actorID, runIndex int64, workflowFile string) actionsdb.WorkflowRun {
	t.Helper()
	row, err := actionsdb.New().InsertWorkflowRun(context.Background(), pool, actionsdb.InsertWorkflowRunParams{
		RepoID:       repoID,
		RunIndex:     runIndex,
		WorkflowFile: workflowFile,
		WorkflowName: "CI",
		HeadSha:      strings.Repeat("a", 40),
		HeadRef:      "trunk",
		Event:        actionsdb.WorkflowRunEventPush,
		EventPayload: []byte(`{}`),
		ActorUserID:  pgtype.Int8{Int64: actorID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertWorkflowRun: %v", err)
	}
	return row
}

func TestActionsRuns_List(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	seedWorkflowRun(t, pool, repoID, userID, 1, ".shithub/workflows/ci.yml")
	seedWorkflowRun(t, pool, repoID, userID, 2, ".shithub/workflows/release.yml")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/runs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiActionsRun
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("len: got %d, want 2; payload=%+v", len(listed), listed)
	}
}

func TestActionsRuns_FilterByWorkflowFile(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	seedWorkflowRun(t, pool, repoID, userID, 1, ".shithub/workflows/ci.yml")
	seedWorkflowRun(t, pool, repoID, userID, 2, ".shithub/workflows/release.yml")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/runs?workflow_file=.shithub/workflows/release.yml", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiActionsRun
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].WorkflowFile != ".shithub/workflows/release.yml" {
		t.Errorf("expected only release.yml; got %+v", listed)
	}
}

func TestActionsRuns_GetSingle(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	run := seedWorkflowRun(t, pool, repoID, userID, 1, ".shithub/workflows/ci.yml")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/runs/"+strconv.FormatInt(run.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiActionsRun
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != run.ID || got.RunNumber != 1 {
		t.Errorf("shape: %+v", got)
	}
}

func TestActionsRuns_GetCrossRepoReturns404(t *testing.T) {
	pool, router, userID, repoID, _ := seedIssuesEnv(t, "alice")
	run := seedWorkflowRun(t, pool, repoID, userID, 1, ".shithub/workflows/ci.yml")

	// Bob can probe under his own repo with alice's run id → 404.
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))
	body, _ := json.Marshal(map[string]any{"name": "playground", "visibility": "public"})
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", strings.NewReader(string(body)))
	createReq.Header.Set("Authorization", "Bearer "+bobToken)
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	router.ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusCreated {
		t.Fatalf("seed bob repo: %d; body=%s", createRR.Code, createRR.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/bob/playground/actions/runs/"+strconv.FormatInt(run.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-repo: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsRuns_GetUnknown404(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/runs/99999", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsRuns_JobsListEmpty(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	run := seedWorkflowRun(t, pool, repoID, userID, 1, ".shithub/workflows/ci.yml")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/runs/"+strconv.FormatInt(run.ID, 10)+"/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiActionsJob
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("expected empty jobs list; got %+v", listed)
	}
}

func TestActionsRuns_RequiresRepoRead(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	wrong := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/runs", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
