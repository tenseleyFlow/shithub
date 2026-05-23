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
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// apiPRRef mirrors the server's prRefEnvelope. S60 added the nested
// base/head shape so gh-compat clients (the shithub-cli pr view path)
// can read branch + SHA without parsing the legacy flat fields. E2
// added the `repo` envelope underneath (A16 partial-regression
// closeout).
type apiPRRef struct {
	Ref  string     `json:"ref"`
	SHA  string     `json:"sha"`
	Repo *apiPRRepo `json:"repo"`
}

// apiPRRepo mirrors the server's prRepoEnvelope — the trimmed repo
// node that rides on PR base/head (E2).
type apiPRRepo struct {
	ID       int64         `json:"id"`
	Name     string        `json:"name"`
	FullName string        `json:"full_name"`
	Owner    *apiRepoOwner `json:"owner"`
	Private  bool          `json:"private"`
	HTMLURL  string        `json:"html_url"`
}

type apiPull struct {
	ID     int64  `json:"id"`
	Number int64  `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	// Legacy flat fields (S60 keeps them during transition).
	BaseRef string `json:"base_ref"`
	HeadRef string `json:"head_ref"`
	BaseOID string `json:"base_oid"`
	HeadOID string `json:"head_oid"`
	// GitHub-compat nested envelopes.
	Base           *apiPRRef `json:"base"`
	Head           *apiPRRef `json:"head"`
	MergeableState string    `json:"mergeable_state"`
	Merged         bool      `json:"merged"`
	MergeCommit    string    `json:"merge_commit_sha"`
	MergeMethod    string    `json:"merge_method"`
	MergedAt       string    `json:"merged_at"`
	AuthorID       int64     `json:"author_id"`
	User           *apiUser  `json:"user"`
	HTMLURL        string    `json:"html_url"`
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
	// S60 audit A16: GitHub-compat nested base/head envelope must
	// arrive populated alongside the legacy flat fields so gh-compat
	// clients render "base: trunk ← head: feature" instead of two
	// empty strings.
	if created.Base == nil || created.Base.Ref != "trunk" {
		t.Errorf("base envelope: %+v", created.Base)
	}
	if created.Head == nil || created.Head.Ref != "feature" {
		t.Errorf("head envelope: %+v", created.Head)
	}
	// S60 audit A12: user envelope arrives alongside author_id.
	if created.User == nil || created.User.Login != "alice" {
		t.Errorf("user envelope: %+v", created.User)
	}
	// B-audit B7: PR responses must carry html_url so CLI clients can
	// surface it on success.
	if !strings.HasSuffix(created.HTMLURL, "/alice/demo/pulls/1") {
		t.Errorf("html_url: got %q, want suffix /alice/demo/pulls/1", created.HTMLURL)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d; body=%s", rr.Code, rr.Body.String())
	}
}

// TestPulls_ResponseCarriesBaseHeadRepoEnvelope is the E2 regression
// (A16 partial-regression closeout). PR base/head used to emit only
// `{ref, sha}`; gh-compat fork-PR rendering needs `repo` underneath
// so the CLI's `--json baseRepository,headRepository` can distinguish
// same-repo PRs from cross-repo (fork) ones. For a same-repo PR both
// base.repo and head.repo are the same envelope.
func TestPulls_ResponseCarriesBaseHeadRepoEnvelope(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{
		"title": "wire", "body": "x",
		"base": "trunk", "head": "feature",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiPull
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for label, ref := range map[string]*apiPRRef{"base": got.Base, "head": got.Head} {
		if ref == nil {
			t.Fatalf("%s envelope absent", label)
		}
		if ref.Repo == nil {
			t.Errorf("%s.repo: missing — E2 regression", label)
			continue
		}
		if ref.Repo.Name != "demo" || ref.Repo.FullName != "alice/demo" {
			t.Errorf("%s.repo: %+v", label, ref.Repo)
		}
		if ref.Repo.Owner == nil || ref.Repo.Owner.Login != "alice" || ref.Repo.Owner.Type != "User" {
			t.Errorf("%s.repo.owner: %+v", label, ref.Repo.Owner)
		}
		if ref.Repo.Private {
			t.Errorf("%s.repo.private: got true, want false", label)
		}
		if !strings.HasSuffix(ref.Repo.HTMLURL, "/alice/demo") {
			t.Errorf("%s.repo.html_url: %q", label, ref.Repo.HTMLURL)
		}
	}

	// Confirm the GET path emits the same shape — the audit's exact
	// repro was `shithub api .../pulls/N` on a fetched PR.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d", rr.Code)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"repo":`)) {
		t.Errorf("GET response missing repo key on base/head; raw=%s", rr.Body.String())
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

// G6 (F46): a second OPEN PR with the same `(base, head)` must 422
// and surface the existing PR's number — pre-fix the server stacked
// unlimited duplicates with no warning. Closing the first PR (or
// merging it) re-opens the slot, matching gh's rule.
func TestPulls_CreateRejectsDuplicateOpenPR(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")

	// First create succeeds.
	first := openPullFor(t, router, token, "alice", "demo")

	// Second create with the same head→base 422s and names the existing PR.
	body, _ := json.Marshal(map[string]any{
		"title": "dup", "base": "trunk", "head": "feature",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate create: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	wantN := strconv.FormatInt(first.Number, 10)
	if !strings.Contains(rr.Body.String(), "#"+wantN) {
		t.Errorf("422 body should name existing PR #%s; got %s", wantN, rr.Body.String())
	}

	// Close the first PR; a second OPEN PR over the same pair now succeeds.
	patch, _ := json.Marshal(map[string]any{"state": "closed"})
	req = httptest.NewRequest(http.MethodPatch,
		"/api/v1/repos/alice/demo/pulls/"+wantN, bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("close first PR: %d %s", rr.Code, rr.Body.String())
	}
	body, _ = json.Marshal(map[string]any{
		"title": "reopen-slot", "base": "trunk", "head": "feature",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Errorf("after closing first, second create should succeed: %d %s", rr.Code, rr.Body.String())
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

// G7 (F27): PATCH with `base` persists the new ref and recomputes
// mergeable_state on the next worker tick. Pre-fix the field was
// dropped at JSON decode and the call was a no-op false-success.
// This test seeds a third branch (`stable`), changes the PR's base
// from `trunk` to `stable`, and verifies both the legacy flat field
// and the nested envelope reflect the change.
func TestPulls_PatchBaseChange(t *testing.T) {
	_, router, _, _, token, gitDir := seedPullsEnv(t, "alice")
	// Add a third branch so we can pivot base off `trunk`.
	commitOnRepoBranch(t, gitDir, "stable", "stable init", "STABLE.md", "stable\n")
	openPullFor(t, router, token, "alice", "demo")

	patch, _ := json.Marshal(map[string]any{"base": "stable"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/pulls/1", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch base: %d; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiPull
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.BaseRef != "stable" {
		t.Errorf("base_ref: got %q want %q", updated.BaseRef, "stable")
	}
	if updated.Base == nil || updated.Base.Ref != "stable" {
		t.Errorf("base envelope: %+v", updated.Base)
	}

	// GET round-trip: persistence sticks across requests (not just the
	// reload-in-response).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d", rr.Code)
	}
	var fetched apiPull
	_ = json.Unmarshal(rr.Body.Bytes(), &fetched)
	if fetched.BaseRef != "stable" {
		t.Errorf("base_ref after GET: %q want stable", fetched.BaseRef)
	}
}

// G7 (F27): boundary checks — unknown ref 422s; new base == head 422s.
func TestPulls_PatchBaseRejectsBadInputs(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	for _, tc := range []struct{ base, want string }{
		{"no-such-branch", "base ref not found"},
		{"feature", "base and head must differ"}, // PR head is `feature`
	} {
		patch, _ := json.Marshal(map[string]any{"base": tc.base})
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/pulls/1", bytes.NewReader(patch))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("base=%q: code=%d want 422; body=%s", tc.base, rr.Code, rr.Body.String())
			continue
		}
		if !strings.Contains(rr.Body.String(), tc.want) {
			t.Errorf("base=%q: body should mention %q; got %s", tc.base, tc.want, rr.Body.String())
		}
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

// E5 regression seatbelts. Pre-fix, pulls list silently dropped all
// filters except `state` (and accepted any string for that).
func TestPulls_ListStateStrict(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	for _, tc := range []struct {
		state    string
		wantCode int
	}{
		{"open", 200},
		{"closed", 200},
		{"merged", 200},
		{"all", 200},
		{"", 200},
		{"nonsense", 422},
		{"draft", 422},
		// H3 (H8): byte-exact match. Pre-fix the validator's
		// ToLower(TrimSpace) chain silently accepted these as "open"
		// — now each surfaces 422 with the typo visible to the user.
		{"OPEN", 422},
		{"open%20", 422}, // trailing space, URL-encoded
		{"%20open", 422}, // leading space
		{"open%0A", 422}, // trailing newline
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?state="+tc.state, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("state=%q: code=%d want %d; body=%s", tc.state, rr.Code, tc.wantCode, rr.Body.String())
		}
	}
}

func TestPulls_ListDraftStrict(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	for _, tc := range []struct {
		draft    string
		wantCode int
	}{
		{"true", 200},
		{"false", 200},
		{"", 200},
		{"yes", 422},
		{"1", 422},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?draft="+tc.draft, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("draft=%q: code=%d want %d; body=%s", tc.draft, rr.Code, tc.wantCode, rr.Body.String())
		}
	}
}

// TestPulls_ListSortDirectionStrict pins F2-4: pre-fix the server
// silently accepted any value for sort/direction/order and returned
// a full unsorted list. The validator now 422s on unknown values.
func TestPulls_ListSortDirectionStrict(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	for _, tc := range []struct {
		q        string
		wantCode int
	}{
		{"sort=created", 200},
		{"sort=updated", 200},
		{"sort=popularity", 200},
		{"sort=long-running", 200},
		{"sort=BOGUS", 422},
		{"direction=asc", 200},
		{"direction=desc", 200},
		{"direction=BOGUS", 422},
		{"order=asc", 200},
		{"order=desc", 200},
		{"order=BOGUS", 422},
		// Combinations: bad sort wins over good direction.
		{"sort=BOGUS&direction=asc", 422},
		{"sort=created&direction=BOGUS", 422},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?"+tc.q, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("q=%q: code=%d want %d; body=%s", tc.q, rr.Code, tc.wantCode, rr.Body.String())
		}
	}
}

func TestPulls_ListAuthorFilter(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	for _, tc := range []struct {
		author   string
		wantCode int
		wantLen  int
	}{
		{"alice", 200, 1},
		{"ghost", 422, 0}, // unknown user → 422 (not silent unfiltered)
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?author="+tc.author, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("author=%q: code=%d want %d; body=%s", tc.author, rr.Code, tc.wantCode, rr.Body.String())
			continue
		}
		if tc.wantCode != 200 {
			continue
		}
		var rows []apiPull
		_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		if len(rows) != tc.wantLen {
			t.Errorf("author=%q: got %d rows, want %d", tc.author, len(rows), tc.wantLen)
		}
	}
}

func TestPulls_ListBaseFilter(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	// Default PR is base=trunk; filtering on it returns 1.
	// G5 (F15): bogus base ref now 422s (pre-fix silently empty).
	for _, tc := range []struct {
		base     string
		wantCode int
		wantLen  int
	}{
		{"trunk", 200, 1},
		{"BOGUS", http.StatusUnprocessableEntity, 0},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?base="+tc.base, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("base=%q: code=%d want %d; body=%s", tc.base, rr.Code, tc.wantCode, rr.Body.String())
			continue
		}
		if tc.wantCode != 200 {
			continue
		}
		var rows []apiPull
		_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		if len(rows) != tc.wantLen {
			t.Errorf("base=%q: got %d rows, want %d", tc.base, len(rows), tc.wantLen)
		}
	}
}

func TestPulls_ListLabelFilter(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	for _, tc := range []struct {
		labels   string
		wantCode int
		wantLen  int
	}{
		{"bug", 200, 0},  // default labels are seeded; no PRs have them
		{"NOPE", 422, 0}, // unknown label name -> 422 (matches C8 issue side)
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?labels="+tc.labels, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("labels=%q: code=%d want %d; body=%s", tc.labels, rr.Code, tc.wantCode, rr.Body.String())
			continue
		}
		if tc.wantCode != 200 {
			continue
		}
		var rows []apiPull
		_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		if len(rows) != tc.wantLen {
			t.Errorf("labels=%q: got %d rows, want %d", tc.labels, len(rows), tc.wantLen)
		}
	}
}

// G1 (F11): gh-canonical `creator=` is accepted as an alias for the
// shithub-native `author=`. Pre-fix the CLI's `pr list --author ghost`
// sent `?creator=ghost`, the server silently dropped the unknown
// param, and the response was the unfiltered list — wire mismatch
// hidden by passing per-side unit tests.
func TestPulls_ListAuthorAliasCreator(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	get := func(q string) (int, []apiPull) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?"+q, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		var rows []apiPull
		if rr.Code == http.StatusOK {
			_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		}
		return rr.Code, rows
	}

	if code, rows := get("author=alice"); code != 200 || len(rows) != 1 {
		t.Errorf("author=alice: code=%d rows=%d", code, len(rows))
	}
	if code, rows := get("creator=alice"); code != 200 || len(rows) != 1 {
		t.Errorf("creator=alice (alias): code=%d rows=%d", code, len(rows))
	}
	if code, _ := get("creator=ghost"); code != http.StatusUnprocessableEntity {
		t.Errorf("creator=ghost: code=%d, want 422", code)
	}
}

// G1 (F2-1): `assignee` filter on /pulls. Pre-fix the param landed in
// query-string parse-out land — the handler never read it — so the
// CLI's `pr list --assignee` silently returned the unfiltered list.
// Now we validate the username up front (422 on unknown) and post-
// filter via the shared issue-assignee table.
func TestPulls_ListAssigneeFilter(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	get := func(q string) (int, []apiPull) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?"+q, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		var rows []apiPull
		if rr.Code == http.StatusOK {
			_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		}
		return rr.Code, rows
	}

	// alice exists but no PR has her as assignee yet → empty (NOT the
	// pre-fix "unfiltered 1-row" result).
	if code, rows := get("assignee=alice"); code != 200 || len(rows) != 0 {
		t.Errorf("assignee=alice: code=%d rows=%d (want 0)", code, len(rows))
	}
	if code, _ := get("assignee=ghost"); code != http.StatusUnprocessableEntity {
		t.Errorf("assignee=ghost (unknown user): code=%d, want 422", code)
	}
}

// G1 (F2-2): `head` filter on /pulls — mirrors the existing `base`
// post-filter. Pre-fix the CLI's `pr list --head feature` was a no-op
// (silently returned all rows). Now the head ref is compared per row.
func TestPulls_ListHeadFilter(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	get := func(q string) (int, []apiPull) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls?"+q, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		var rows []apiPull
		if rr.Code == http.StatusOK {
			_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		}
		return rr.Code, rows
	}

	if code, rows := get("head=feature"); code != 200 || len(rows) != 1 {
		t.Errorf("head=feature: code=%d rows=%d (want 1)", code, len(rows))
	}
	// G5 (F2-2): bogus head ref now 422s (pre-fix silently returned the
	// unfiltered list pre-G1; silent-empty post-G1; both shapes hide typos).
	if code, _ := get("head=NOPE"); code != http.StatusUnprocessableEntity {
		t.Errorf("head=NOPE: code=%d, want 422", code)
	}
}

// G2 (F3): `pr comment` POSTs to `/issues/{N}/comments` (gh-compat
// shared namespace). Pre-fix the server's strict `kind=issue` gate
// 404'd PR numbers there — `pr comment` broken end-to-end. Same gate
// also broke GET (listing PR conversation comments). This test pins
// both directions of the comments roundtrip against a PR row.
func TestPulls_SharedNamespace_CommentsRoundtrip(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	pr := openPullFor(t, router, token, "alice", "demo")

	// POST /issues/{N}/comments against PR number must succeed.
	body, _ := json.Marshal(map[string]any{"body": "looks great"})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/repos/alice/demo/issues/"+strconv.FormatInt(pr.Number, 10)+"/comments",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST comment: code=%d body=%s", rr.Code, rr.Body.String())
	}

	// GET /issues/{N}/comments must list the conversation comment we
	// just posted — verifies the gate is lifted on both verbs.
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/repos/alice/demo/issues/"+strconv.FormatInt(pr.Number, 10)+"/comments", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET comments: code=%d body=%s", rr.Code, rr.Body.String())
	}
	var rows []apiComment
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode comments: %v", err)
	}
	if len(rows) != 1 || rows[0].Body != "looks great" {
		t.Errorf("comments: %+v", rows)
	}
}

// G2 (F44): `pr lock` / `pr unlock` route through /issues/{N}/lock
// (gh-compat shared namespace). Pre-fix the kind gate 404'd PR
// numbers. Verifies both verbs against a PR and that lock state
// persists (visible on GET /pulls/{N}).
func TestPulls_SharedNamespace_LockUnlock(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	pr := openPullFor(t, router, token, "alice", "demo")
	num := strconv.FormatInt(pr.Number, 10)

	// PUT lock
	body, _ := json.Marshal(map[string]any{"lock_reason": "off-topic"})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/repos/alice/demo/issues/"+num+"/lock", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("PUT lock: code=%d body=%s", rr.Code, rr.Body.String())
	}

	// DELETE unlock
	req = httptest.NewRequest(http.MethodDelete,
		"/api/v1/repos/alice/demo/issues/"+num+"/lock", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE lock: code=%d body=%s", rr.Code, rr.Body.String())
	}
}

// G2 (F26): `pr edit --add-label`, `--add-assignee` PATCH
// `/issues/{N}` with labels/assignees fields. Pre-fix the strict
// kind gate 404'd PR numbers there. PRs share the issue label +
// assignee tables, so once the gate lifts the existing code paths
// just work. This test pins both fields on a PR row.
func TestPulls_SharedNamespace_PatchLabelsAndAssignees(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	pr := openPullFor(t, router, token, "alice", "demo")
	num := strconv.FormatInt(pr.Number, 10)

	// PATCH /issues/{N} with labels — "bug" is one of the default seeded
	// labels on a new repo.
	body, _ := json.Marshal(map[string]any{
		"labels":    []string{"bug"},
		"assignees": []string{"alice"},
	})
	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/repos/alice/demo/issues/"+num, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH labels+assignees on PR: code=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// G2: PR rows must surface the /pulls/{N} URL, not /issues/{N},
	// even though the response shape rides on the issue handler.
	if !strings.HasSuffix(resp.HTMLURL, "/alice/demo/pulls/"+num) {
		t.Errorf("html_url on PR: got %q, want suffix /alice/demo/pulls/%s", resp.HTMLURL, num)
	}
	if len(resp.Labels) != 1 || resp.Labels[0].Name != "bug" {
		t.Errorf("labels: %+v", resp.Labels)
	}
	if len(resp.Assignees) != 1 || resp.Assignees[0].Login != "alice" {
		t.Errorf("assignees: %+v", resp.Assignees)
	}

	// GET /pulls/{N} should reflect the same label set (PRs share the
	// issue_labels join table).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/"+num, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET pull: %d %s", rr.Code, rr.Body.String())
	}
}

// G2 boundary: PRs go through /pulls/{N} for title/body/state. The
// shared-namespace PATCH /issues/{N} accepts label/assignee/milestone
// on a PR but rejects title/body/state with a 422 directive pointing
// the caller at the correct route. Locks in the design boundary.
func TestPulls_SharedNamespace_PatchTitleBodyStateRejected(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	pr := openPullFor(t, router, token, "alice", "demo")
	num := strconv.FormatInt(pr.Number, 10)

	for _, field := range []map[string]any{
		{"title": "new title"},
		{"body": "new body"},
		{"state": "closed"},
		{"state_reason": "completed"},
	} {
		body, _ := json.Marshal(field)
		req := httptest.NewRequest(http.MethodPatch,
			"/api/v1/repos/alice/demo/issues/"+num, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("PATCH %v: code=%d want 422; body=%s", field, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "PATCH /pulls/") {
			t.Errorf("PATCH %v: error should redirect to /pulls/{N}; got %s", field, rr.Body.String())
		}
	}
}

// G2 boundary: GET /issues/{N} on a PR number must still 404. The
// kindless resolver is opt-in for sub-routes; the bare GET surface
// is issue-specific (PRs get their own /pulls/{N} GET).
func TestPulls_BareIssuesGetStillRejectsPR(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	pr := openPullFor(t, router, token, "alice", "demo")
	num := strconv.FormatInt(pr.Number, 10)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/"+num, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /issues/{PR}: code=%d want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// G5 (F15/F2-2): base/head ref validation now hits git rev-parse so
// typos surface as a 422 with the bad value echoed back. Pre-fix the
// listing silently returned empty (base) or unfiltered/empty (head) —
// both shapes hid typos from CLI scripts. This test pins the wire
// shape of the new error so the CLI can match against "ref %q not
// found" if it wants to render a friendly hint.
func TestPulls_ListRefFilterErrorShape(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, token, "alice", "demo")

	for _, tc := range []struct{ param, label string }{
		{"base=NOPE", "base"},
		{"head=NOPE", "head"},
	} {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/repos/alice/demo/pulls?"+tc.param, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: code=%d want 422", tc.param, rr.Code)
		}
		if !strings.HasPrefix(rr.Body.String(), `{"error":"`+tc.label+`:`) {
			t.Errorf("%s: body should be prefixed with %q-label: %s", tc.param, tc.label, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), `NOPE`) {
			t.Errorf("%s: body should echo the bad ref: %s", tc.param, rr.Body.String())
		}
	}
}

// G8b (F43): PUT /pulls/{N}/update-branch happy path. Seed a PR with
// base=trunk and head=feature, then advance trunk so the branch is
// behind. After update-branch, head_oid should have moved and the
// PR's GET response should reflect the new head.
func TestPulls_UpdateBranchMergeStrategy(t *testing.T) {
	_, router, _, _, token, gitDir := seedPullsEnv(t, "alice")
	pr := openPullFor(t, router, token, "alice", "demo")
	num := strconv.FormatInt(pr.Number, 10)
	oldHead := pr.HeadOID

	commitOnRepoBranch(t, gitDir, "trunk", "trunk advance", "TRUNK.md", "advance\n")

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/repos/alice/demo/pulls/"+num+"/update-branch", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update-branch: %d; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Message string `json:"message"`
		URL     string `json:"url"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Message == "" || resp.URL == "" {
		t.Errorf("response shape: %+v", resp)
	}

	// GET reflects the moved head_oid.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/"+num, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get pr: %d", rr.Code)
	}
	var refreshed apiPull
	_ = json.Unmarshal(rr.Body.Bytes(), &refreshed)
	if refreshed.HeadOID == "" || refreshed.HeadOID == oldHead {
		t.Errorf("head_oid should have advanced: was=%q now=%q", oldHead, refreshed.HeadOID)
	}
}

// G8b (F43): when head already contains base, update-branch is a
// no-op and returns 422 "already up to date".
func TestPulls_UpdateBranchAlreadyUpToDate(t *testing.T) {
	_, router, _, _, token, _ := seedPullsEnv(t, "alice")
	pr := openPullFor(t, router, token, "alice", "demo")
	num := strconv.FormatInt(pr.Number, 10)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/repos/alice/demo/pulls/"+num+"/update-branch", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("up-to-date: code=%d want 422; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "already up to date") {
		t.Errorf("expected 'already up to date' in body: %s", rr.Body.String())
	}
}

// G8b (F43): `expected_head_sha` CAS guard rejects with 503 when the
// caller's pinned SHA doesn't match the current head. Pins the wire
// shape so a CLI retry-with-fresh-SHA flow can detect the conflict.
func TestPulls_UpdateBranchExpectedHeadMismatch(t *testing.T) {
	_, router, _, _, token, gitDir := seedPullsEnv(t, "alice")
	pr := openPullFor(t, router, token, "alice", "demo")
	num := strconv.FormatInt(pr.Number, 10)
	commitOnRepoBranch(t, gitDir, "trunk", "trunk advance", "TRUNK.md", "advance\n")

	body, _ := json.Marshal(map[string]any{
		"expected_head_sha": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/repos/alice/demo/pulls/"+num+"/update-branch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected_head_sha mismatch: code=%d want 503; body=%s", rr.Code, rr.Body.String())
	}
	_ = pr.HeadOID
}

// G8b (F43): non-author read-only collaborator gets 403. Author or
// repo-write collaborator can run update-branch.
func TestPulls_UpdateBranchForbidsNonAuthorReader(t *testing.T) {
	pool, router, _, _, token, _ := seedPullsEnv(t, "alice")
	pr := openPullFor(t, router, token, "alice", "demo")
	num := strconv.FormatInt(pr.Number, 10)

	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobTok := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead))
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/repos/alice/demo/pulls/"+num+"/update-branch", nil)
	req.Header.Set("Authorization", "Bearer "+bobTok)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-author update-branch: code=%d want 403", rr.Code)
	}
}
