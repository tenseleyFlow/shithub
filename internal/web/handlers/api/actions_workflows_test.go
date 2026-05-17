// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

func ownerIDForAlice(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	u, err := usersdb.New().GetUserByUsername(context.Background(), pool, "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	return u.ID
}

type apiWorkflow struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	File  string `json:"file"`
	State string `json:"state"`
}

const ciYAML = `name: CI
on:
  push:
  workflow_dispatch:
    inputs:
      env:
        type: choice
        options: [qa, prod]
        default: qa
      debug:
        type: boolean
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

const noDispatchYAML = `name: PushOnly
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

func seedRepoWithWorkflow(t *testing.T, gitDir string, files map[string]string) string {
	t.Helper()
	entries := []repogit.FileEntry{{Path: "README.md", Body: []byte("# demo\n")}}
	for path, body := range files {
		entries = append(entries, repogit.FileEntry{Path: path, Body: []byte(body)})
	}
	commit, err := (repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.test",
		Branch:      "trunk",
		Message:     "init",
		When:        time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
		Files:       entries,
	}).Build(context.Background())
	if err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	return commit
}

func TestActionsWorkflows_ListReturnsDiscoveredFiles(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/ci.yml":      ciYAML,
		".shithub/workflows/release.yml": noDispatchYAML,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/workflows", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	// S62 envelope: {total_count, workflows: [...]}
	var envelope struct {
		TotalCount int           `json:"total_count"`
		Workflows  []apiWorkflow `json:"workflows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	listed := envelope.Workflows
	if envelope.TotalCount != 2 || len(listed) != 2 {
		t.Fatalf("expected 2 workflows; got %+v (total=%d)", listed, envelope.TotalCount)
	}
	byPath := map[string]apiWorkflow{}
	for _, w := range listed {
		byPath[w.Path] = w
	}
	if w := byPath[".shithub/workflows/ci.yml"]; w.Name != "CI" || w.File != "ci.yml" || w.State != "active" {
		t.Errorf("ci.yml shape: %+v", w)
	}
	if w := byPath[".shithub/workflows/release.yml"]; w.Name != "PushOnly" || w.File != "release.yml" {
		t.Errorf("release.yml shape: %+v", w)
	}
}

func TestActionsWorkflows_ListEmptyRepoReturnsEmptyArray(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedBranches(t, gitDir, nil, nil) // initial commit, no workflows

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/workflows", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	// S62 envelope: empty list returns {"total_count":0,"workflows":[]}.
	var env struct {
		TotalCount int           `json:"total_count"`
		Workflows  []apiWorkflow `json:"workflows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if env.TotalCount != 0 || len(env.Workflows) != 0 {
		t.Errorf("empty list expected total=0/[]; got total=%d workflows=%v", env.TotalCount, env.Workflows)
	}
}

func TestActionsWorkflows_GetByFileName(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/ci.yml": ciYAML,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/workflows/ci.yml", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiWorkflow
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Name != "CI" || got.File != "ci.yml" {
		t.Errorf("shape: %+v", got)
	}
	if got.ID == 0 {
		t.Errorf("missing id: %+v", got)
	}
}

func TestActionsWorkflows_GetUnknown404(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/ci.yml": ciYAML,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/workflows/ghost.yml", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsWorkflows_DispatchHappyPath(t *testing.T) {
	pool, router, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/ci.yml": ciYAML,
	})
	// repo:write scope needed for dispatch; seedBranchesEnv mints repo:read.
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{
		"ref": "trunk",
		"inputs": map[string]string{
			"env":   "prod",
			"debug": "true",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/actions/workflows/ci.yml/dispatches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsWorkflows_DispatchRejectsBadInput(t *testing.T) {
	pool, router, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/ci.yml": ciYAML,
	})
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{
		"inputs": map[string]string{"env": "staging"}, // not in choice options
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/actions/workflows/ci.yml/dispatches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsWorkflows_DispatchRejectsWorkflowWithoutDispatch(t *testing.T) {
	pool, router, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/push.yml": noDispatchYAML,
	})
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/actions/workflows/push.yml/dispatches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsWorkflows_DispatchRequiresRepoWrite(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/ci.yml": ciYAML,
	})
	// token from seedBranchesEnv is repo:read only.
	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/actions/workflows/ci.yml/dispatches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsWorkflows_DispatchRejectsUnknownWorkflow(t *testing.T) {
	pool, router, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/ci.yml": ciYAML,
	})
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/actions/workflows/ghost.yml/dispatches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}
