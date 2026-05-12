// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

type apiPull struct {
	ID             int64  `json:"id"`
	Number         int64  `json:"number"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	BaseRef        string `json:"base_ref"`
	HeadRef        string `json:"head_ref"`
	BaseOID        string `json:"base_oid"`
	HeadOID        string `json:"head_oid"`
	MergeableState string `json:"mergeable_state"`
	Merged         bool   `json:"merged"`
	MergeCommit    string `json:"merge_commit_sha"`
	MergeMethod    string `json:"merge_method"`
	MergedAt       string `json:"merged_at"`
	AuthorID       int64  `json:"author_id"`
}

// gitCmdAPI is the test-side git shell wrapper — every invocation runs
// against a t.TempDir path the test set up.
//
//nolint:gosec
func gitCmdAPI(args ...string) *exec.Cmd { return exec.Command("git", args...) }

// commitOnRepoBranch lands a commit on `branch` of the bare repo at
// gitDir via a temp worktree. Branch is created if missing.
func commitOnRepoBranch(t *testing.T, gitDir, branch, msg, file, contents string) string {
	t.Helper()
	wt := t.TempDir()
	addArgs := []string{"-C", gitDir, "worktree", "add"}
	if _, err := gitCmdAPI("-C", gitDir, "show-ref", "--verify", "refs/heads/"+branch).CombinedOutput(); err != nil {
		addArgs = append(addArgs, "-b", branch, wt)
	} else {
		addArgs = append(addArgs, wt, branch)
	}
	if out, err := gitCmdAPI(addArgs...).CombinedOutput(); err != nil {
		t.Fatalf("worktree add %s: %v (%s)", branch, err, out)
	}
	defer func() {
		_ = gitCmdAPI("-C", gitDir, "worktree", "remove", "--force", wt).Run()
	}()

	if err := os.WriteFile(filepath.Join(wt, file), []byte(contents), 0o644); err != nil { //nolint:gosec
		t.Fatalf("write %s: %v", file, err)
	}
	for _, args := range [][]string{
		{"-C", wt, "config", "user.name", "Alice"},
		{"-C", wt, "config", "user.email", "alice@example.com"},
		{"-C", wt, "add", "."},
		{"-C", wt, "commit", "-m", msg},
	} {
		if out, err := gitCmdAPI(args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, out)
		}
	}
	out, err := gitCmdAPI("-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// seedPullsEnv builds the full PR-test environment: pool, router, owner
// user, an `alice/demo` repo initialized through repos.Create, a
// `trunk` commit, a `feature` commit, and a PAT scoped to repo:write.
// gitDir is returned so individual tests can add more commits when
// needed (e.g. to dirty the mergeable_state).
func seedPullsEnv(t *testing.T, ownerUsername string) (pool *pgxpool.Pool, router http.Handler, userID, repoID int64, token, gitDir string) {
	t.Helper()
	pool = dbtest.NewTestDB(t)
	router, rfs := newReposAPIRouter(t, pool)
	userID = seedRepoCreatorUser(t, pool, ownerUsername)
	token = mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	res, err := repos.Create(context.Background(), repos.Deps{
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
	})
	if err != nil {
		t.Fatalf("repos.Create: %v", err)
	}
	repoID = res.Repo.ID
	gitDir, err = rfs.RepoPath(ownerUsername, "demo")
	if err != nil {
		t.Fatalf("RepoFS.RepoPath: %v", err)
	}
	commitOnRepoBranch(t, gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnRepoBranch(t, gitDir, "feature", "add foo", "foo.txt", "foo\n")
	return pool, router, userID, repoID, token, gitDir
}

func TestPulls_CreateAndGet(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{
		"title": "wire up foo", "body": "first cut",
		"base": "trunk", "head": "feature",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d; body=%s", rr.Code, rr.Body.String())
	}
	var created apiPull
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Title != "wire up foo" || created.BaseRef != "trunk" || created.HeadRef != "feature" {
		t.Errorf("shape: %+v", created)
	}
	if created.State != "open" || created.Merged {
		t.Errorf("state: %+v", created)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPulls_CreateRejectsSameBranch(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "self", "base": "trunk", "head": "trunk"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPulls_CreateRejectsMissingHead(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "ghost", "base": "trunk", "head": "no-such-branch"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPulls_CreateRequiresRepoWriteScope(t *testing.T) {
	pool, router, userID, _, _, _ := seedPullsEnv(t, "alice")
	readOnly := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	body, _ := json.Marshal(map[string]any{"title": "x", "base": "trunk", "head": "feature"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+readOnly)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPulls_PatchTitleBody(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")

	openPullFor(t, router, token, "alice", "demo")

	patch, _ := json.Marshal(map[string]any{"title": "renamed", "body": "renamed body"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/pulls/1", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: %d; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiPull
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Title != "renamed" || updated.Body != "renamed body" {
		t.Errorf("patch shape: %+v", updated)
	}
}

func TestPulls_PatchNonAuthorForbidden(t *testing.T) {
	pool, router, _, _, tokenAlice, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, tokenAlice, "alice", "demo")

	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	patch, _ := json.Marshal(map[string]any{"title": "hijack"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/pulls/1", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPulls_PatchDraftToReady(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{
		"title": "draft pr", "base": "trunk", "head": "feature", "draft": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("draft create: %d; body=%s", rr.Code, rr.Body.String())
	}

	patch, _ := json.Marshal(map[string]any{"draft": false})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/pulls/1", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("flip draft: %d; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiPull
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	if updated.Draft {
		t.Errorf("expected draft=false; got %+v", updated)
	}
}

func TestPulls_ListFiltersByState(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?state=open", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	var listed []apiPull
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 {
		t.Errorf("open count: got %d, want 1", len(listed))
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?state=closed", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("closed list: %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode closed: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("closed count: got %d, want 0", len(listed))
	}
}

func TestPulls_CommitsAndFilesListed(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1/commits", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("commits: %d; body=%s", rr.Code, rr.Body.String())
	}
	var commits []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &commits); err != nil {
		t.Fatalf("decode commits: %v", err)
	}
	if len(commits) == 0 {
		t.Errorf("expected at least one commit on feature; got %+v", commits)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1/files", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("files: %d; body=%s", rr.Code, rr.Body.String())
	}
	var files []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &files); err != nil {
		t.Fatalf("decode files: %v", err)
	}
	if len(files) == 0 {
		t.Errorf("expected at least one changed file; got %+v", files)
	}
}

func TestPulls_GetReturns404ForNonPRNumber(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"title": "plain issue"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed issue: %d; body=%s", rr.Code, rr.Body.String())
	}

	// Issue #1 is an issue, not a pull request — /pulls/1 must 404.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// openPullFor creates a default `trunk` <- `feature` PR on the supplied
// repo. Fails the test on non-201 so callers can keep their bodies
// short.
func openPullFor(t *testing.T, router http.Handler, token, owner, repo string) apiPull {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"title": "default", "base": "trunk", "head": "feature"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+owner+"/"+repo+"/pulls", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("openPullFor: %d; body=%s", rr.Code, rr.Body.String())
	}
	var out apiPull
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}
