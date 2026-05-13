// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

type apiCommitAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type apiCommit struct {
	SHA          string                `json:"sha"`
	ShortSHA     string                `json:"short_sha"`
	Subject      string                `json:"subject"`
	Body         string                `json:"body"`
	Author       apiCommitAuthor       `json:"author"`
	Verification apiCommitVerification `json:"verification"`
}

type apiCommitVerification struct {
	Verified   bool    `json:"verified"`
	Reason     string  `json:"reason"`
	Signature  *string `json:"signature"`
	Payload    *string `json:"payload"`
	VerifiedAt *string `json:"verified_at"`
}

type apiCommitFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Adds    int    `json:"additions"`
	Deletes int    `json:"deletions"`
}

type apiCommitDetail struct {
	apiCommit
	Committer apiCommitAuthor `json:"committer"`
	Parents   []string        `json:"parents"`
	TreeSHA   string          `json:"tree_sha"`
	Files     []apiCommitFile `json:"files"`
	Stats     struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
		Total     int `json:"total"`
	} `json:"stats"`
}

// seedCommit puts a single commit on `trunk` and returns its SHA.
func seedCommit(t *testing.T, gitDir string) string {
	t.Helper()
	sha, err := (repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.test",
		Branch:      "trunk",
		Message:     "first commit\n\nlonger body for the test",
		When:        time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
		Files: []repogit.FileEntry{
			{Path: "README.md", Body: []byte("# demo\n")},
			{Path: "src/main.go", Body: []byte("package main\n\nfunc main() {}\n")},
		},
	}).Build(context.Background())
	if err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	return sha
}

// commitsEnv stands up a router + repo and seeds one commit; mirrors
// the branches-test pattern.
func commitsEnv(t *testing.T) (router http.Handler, token, headSHA string) {
	t.Helper()
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	headSHA = seedCommit(t, gitDir)
	return router, token, headSHA
}

func TestCommits_ListReturnsHead(t *testing.T) {
	router, token, headSHA := commitsEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/commits", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiCommit
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len: got %d, want 1; payload=%+v", len(listed), listed)
	}
	if listed[0].SHA != headSHA {
		t.Errorf("sha: got %q, want %q", listed[0].SHA, headSHA)
	}
	if listed[0].Subject != "first commit" {
		t.Errorf("subject: %q", listed[0].Subject)
	}
	if listed[0].Author.Name != "Alice" {
		t.Errorf("author: %+v", listed[0].Author)
	}
}

func TestCommits_GetSingleByFullSHA(t *testing.T) {
	router, token, headSHA := commitsEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/commits/"+headSHA, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiCommitDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SHA != headSHA {
		t.Errorf("sha: got %q, want %q", got.SHA, headSHA)
	}
	if got.TreeSHA == "" {
		t.Errorf("tree_sha empty: %+v", got)
	}
	if len(got.Files) != 2 {
		t.Errorf("files: got %d, want 2; payload=%+v", len(got.Files), got.Files)
	}
	if got.Stats.Additions <= 0 {
		t.Errorf("stats.additions: %+v", got.Stats)
	}
}

func TestCommits_GetByShortSHA(t *testing.T) {
	router, token, headSHA := commitsEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/commits/"+headSHA[:7], nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiCommitDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SHA != headSHA {
		t.Errorf("resolved sha: got %q, want %q", got.SHA, headSHA)
	}
}

func TestCommits_GetUnknown404(t *testing.T) {
	router, token, _ := commitsEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCommits_EmptyRepoReturnsEmpty(t *testing.T) {
	// seedBranchesEnv creates a repo but doesn't push a commit; that
	// matches an "uninitialised" state for this test.
	_, router, _, token, _, _ := seedBranchesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/commits", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiCommit
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("expected empty list; got %+v", listed)
	}
}

func TestCommits_RequiresReadScope(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	userID := seedRepoCreatorUser(t, pool, "bob")
	wrong := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/commits", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// TestCommits_VerificationDefaultsUnsigned ensures every commit's
// JSON carries the verification object even when no cache row
// exists. gh emits `{verified: false, reason: "unsigned", signature:
// null, payload: null, verified_at: null}` for commits with no
// signature; we match that exactly.
func TestCommits_VerificationDefaultsUnsigned(t *testing.T) {
	router, token, headSHA := commitsEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/commits", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var listed []apiCommit
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len: got %d, want 1", len(listed))
	}
	if listed[0].SHA != headSHA {
		t.Errorf("sha: got %q, want %q", listed[0].SHA, headSHA)
	}
	if listed[0].Verification.Reason != "unsigned" {
		t.Errorf("reason: got %q, want unsigned", listed[0].Verification.Reason)
	}
	if listed[0].Verification.Verified {
		t.Error("expected Verified=false on an unsigned commit")
	}
	if listed[0].Verification.Signature != nil {
		t.Error("expected Signature=null on an unsigned commit")
	}
	if listed[0].Verification.Payload != nil {
		t.Error("expected Payload=null on an unsigned commit")
	}

	// Single-commit GET surfaces the same object shape.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/commits/"+headSHA, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}
	var single apiCommit
	if err := json.Unmarshal(rr.Body.Bytes(), &single); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if single.Verification.Reason != "unsigned" {
		t.Errorf("get reason: got %q, want unsigned", single.Verification.Reason)
	}
}
