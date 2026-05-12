// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

type apiBranch struct {
	Name      string `json:"name"`
	CommitSHA string `json:"commit_sha"`
	Protected bool   `json:"protected"`
	IsDefault bool   `json:"is_default"`
}

type apiTag struct {
	Name      string `json:"name"`
	CommitSHA string `json:"commit_sha"`
}

// seedBranches mints an initial commit on `trunk` then optionally
// creates extra branches/tags via plumbing so we get a stable ref
// listing the API can enumerate.
func seedBranches(t *testing.T, gitDir string, extraBranches, tags []string) string {
	t.Helper()
	commit, err := (repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.test",
		Branch:      "trunk",
		Message:     "init",
		When:        time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
		Files: []repogit.FileEntry{
			{Path: "README.md", Body: []byte("# demo\n")},
		},
	}).Build(context.Background())
	if err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	for _, name := range extraBranches {
		if out, err := exec.Command("git", "-C", gitDir, "update-ref", "refs/heads/"+name, commit).CombinedOutput(); err != nil {
			t.Fatalf("create branch %q: %v; out=%s", name, err, out)
		}
	}
	for _, name := range tags {
		if out, err := exec.Command("git", "-C", gitDir, "update-ref", "refs/tags/"+name, commit).CombinedOutput(); err != nil {
			t.Fatalf("create tag %q: %v; out=%s", name, err, out)
		}
	}
	return commit
}

// seedBranchesEnv is the branches-test counterpart to seedIssuesEnv —
// returns the router AND the on-disk RepoFS so tests can drop refs
// into the bare repo before hitting the API.
func seedBranchesEnv(t *testing.T, ownerUsername string) (pool *pgxpool.Pool, router http.Handler, rfs *storage.RepoFS, token, owner, repoName string) {
	t.Helper()
	pool = dbtest.NewTestDB(t)
	router, rfs = newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, ownerUsername)
	token = mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	if _, err := repos.Create(context.Background(), repos.Deps{
		Pool:    pool,
		RepoFS:  rfs,
		Audit:   audit.NewRecorder(),
		Limiter: throttle.NewLimiter(),
	}, repos.Params{
		ActorUserID:   userID,
		OwnerUserID:   userID,
		OwnerUsername: ownerUsername,
		Name:          "demo",
		Description:   "demo",
		Visibility:    "public",
	}); err != nil {
		t.Fatalf("repos.Create: %v", err)
	}
	return pool, router, rfs, token, ownerUsername, "demo"
}

func TestBranches_ListIncludesDefault(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedBranches(t, gitDir, []string{"feature/x"}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/branches", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiBranch
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("len: got %d, want 2 (trunk+feature/x); payload=%+v", len(listed), listed)
	}
	var foundTrunk, foundFeature bool
	for _, b := range listed {
		if b.Name == "trunk" {
			foundTrunk = true
			if !b.IsDefault {
				t.Errorf("trunk should be default: %+v", b)
			}
		}
		if b.Name == "feature/x" {
			foundFeature = true
		}
	}
	if !foundTrunk || !foundFeature {
		t.Errorf("missing expected branches: %+v", listed)
	}
}

func TestBranches_GetSingle(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	commit := seedBranches(t, gitDir, []string{"release/v1.0"}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/branches/release/v1.0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiBranch
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "release/v1.0" || got.CommitSHA != commit {
		t.Errorf("shape: %+v", got)
	}
	if got.IsDefault {
		t.Errorf("release/v1.0 should not be default: %+v", got)
	}
}

func TestBranches_GetUnknownReturns404(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedBranches(t, gitDir, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/branches/ghost", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestTags_ListIncludesSeeded(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedBranches(t, gitDir, nil, []string{"v0.1.0", "v0.2.0"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/tags", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiTag
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("len: got %d, want 2; payload=%+v", len(listed), listed)
	}
}

func TestBranches_RequiresReadScope(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	// Mint a wrong-scope token (user:read instead of repo:read) → 403.
	userID := seedRepoCreatorUser(t, pool, "bob")
	tokenNoScope := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/branches", nil)
	req.Header.Set("Authorization", "Bearer "+tokenNoScope)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
