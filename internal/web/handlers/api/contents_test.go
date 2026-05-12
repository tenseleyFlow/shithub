// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

type apiContent struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Size      int64  `json:"size"`
	SHA       string `json:"sha"`
	Encoding  string `json:"encoding"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
}

// seedContentsCommit puts a tree with one README + a src/ subdir.
func seedContentsCommit(t *testing.T, gitDir string) string {
	t.Helper()
	sha, err := (repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.test",
		Branch:      "trunk",
		Message:     "seed",
		When:        time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
		Files: []repogit.FileEntry{
			{Path: "README.md", Body: []byte("# demo\n\nhello world\n")},
			{Path: "src/main.go", Body: []byte("package main\n\nfunc main() {}\n")},
			{Path: "src/util/helper.go", Body: []byte("package util\n")},
		},
	}).Build(context.Background())
	if err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	return sha
}

func TestContents_RootDir(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedContentsCommit(t, gitDir)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/contents", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiContent
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Expect 2 root entries: src/ (dir) + README.md (file). Dirs first.
	if len(listed) != 2 {
		t.Fatalf("len: got %d, want 2; payload=%+v", len(listed), listed)
	}
	if listed[0].Type != "dir" || listed[0].Name != "src" {
		t.Errorf("first entry should be src/ dir: %+v", listed[0])
	}
	if listed[1].Type != "file" || listed[1].Name != "README.md" {
		t.Errorf("second entry should be README.md: %+v", listed[1])
	}
	if listed[1].Path != "README.md" {
		t.Errorf("path: %q", listed[1].Path)
	}
}

func TestContents_NestedDir(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, _ := rfs.RepoPath("alice", "demo")
	seedContentsCommit(t, gitDir)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/contents/src", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiContent
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("len: got %d, want 2 (util/ + main.go); payload=%+v", len(listed), listed)
	}
	// path is fully-qualified
	for _, e := range listed {
		if !strings.HasPrefix(e.Path, "src/") {
			t.Errorf("expected src/ prefix: %+v", e)
		}
	}
}

func TestContents_FileReturnsBase64(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, _ := rfs.RepoPath("alice", "demo")
	seedContentsCommit(t, gitDir)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/contents/README.md", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiContent
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != "file" || got.Encoding != "base64" {
		t.Errorf("shape: %+v", got)
	}
	if got.Truncated {
		t.Errorf("small file unexpectedly truncated: %+v", got)
	}
	body, err := base64.StdEncoding.DecodeString(got.Content)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if !strings.Contains(string(body), "hello world") {
		t.Errorf("body: %q", string(body))
	}
	if got.Binary {
		t.Errorf("UTF-8 README should not be flagged binary: %+v", got)
	}
}

func TestContents_FileAtRef(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, _ := rfs.RepoPath("alice", "demo")
	commitSHA := seedContentsCommit(t, gitDir)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/contents/README.md?ref="+commitSHA, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiContent
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Type != "file" {
		t.Errorf("type: %q", got.Type)
	}
}

func TestContents_UnknownPath404(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, _ := rfs.RepoPath("alice", "demo")
	seedContentsCommit(t, gitDir)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/contents/ghost.txt", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestContents_RequiresReadScope(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	userID := seedRepoCreatorUser(t, pool, "bob")
	wrong := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/contents", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
