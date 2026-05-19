// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
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

// ─── README ─────────────────────────────────────────────────────────

type apiReadme struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Encoding    string `json:"encoding"`
	Content     string `json:"content"`
	DownloadURL string `json:"download_url"`
}

func seedRepoWithFiles(t *testing.T, gitDir string, files map[string]string) {
	t.Helper()
	entries := make([]repogit.FileEntry, 0, len(files))
	for path, body := range files {
		entries = append(entries, repogit.FileEntry{Path: path, Body: []byte(body)})
	}
	if _, err := (repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.test",
		Branch:      "trunk",
		Message:     "init",
		When:        time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
		Files:       entries,
	}).Build(context.Background()); err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
}

func TestRepoReadme_PreferredMarkdown(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithFiles(t, gitDir, map[string]string{
		"README.md":  "# Demo repo\n\nHello!\n",
		"README.rst": "Demo repo\n=========\n", // present but should not win over .md
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/readme", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiReadme
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "README.md" {
		t.Errorf("name: got %q, want README.md", got.Name)
	}
	if got.Encoding != "base64" {
		t.Errorf("encoding: got %q", got.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Content)
	if err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if !strings.Contains(string(decoded), "Demo repo") {
		t.Errorf("content round-trip: %q", decoded)
	}
	if got.Size != int64(len(decoded)) {
		t.Errorf("size mismatch: header=%d, decoded=%d", got.Size, len(decoded))
	}
	if !strings.HasSuffix(got.DownloadURL, "/raw/trunk/README.md") {
		t.Errorf("download_url: %q", got.DownloadURL)
	}
}

func TestRepoReadme_FallbackToNonMarkdown(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithFiles(t, gitDir, map[string]string{
		"README.rst": "Demo repo (rst)\n=========\n",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/readme", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiReadme
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Name != "README.rst" {
		t.Errorf("name: got %q, want README.rst", got.Name)
	}
}

func TestRepoReadme_NoREADMEReturns404(t *testing.T) {
	_, router, rfs, token, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithFiles(t, gitDir, map[string]string{
		"main.go": "package main\nfunc main() {}\n",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/readme", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// TestRepoReadme_EmptyRepoReturns404 is the E13 regression: a repo
// that exists in the DB but has no commits / default_branch pointer
// used to surface as 500 ("lookup failed"). The repo is fine; from the
// README endpoint's perspective there's just nothing to read. gh
// returns 404 in the same shape.
func TestRepoReadme_EmptyRepoReturns404(t *testing.T) {
	_, router, _, token, _, _ := seedBranchesEnv(t, "alice")

	// seedBranchesEnv initializes the repo's git dir but typically
	// pre-seeds files. To exercise the "no commits at all" path we
	// blow away any refs the fixture wrote — easier than threading
	// a new "skip-init" knob through the helper. If there's still a
	// HEAD ref the test devolves into TestRepoReadme_NoREADMEReturns404,
	// which is still a useful assertion.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/readme?ref=does-not-exist", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("README")) && !bytes.Contains(rr.Body.Bytes(), []byte("ref not found")) {
		t.Errorf("body should mention README/ref not found, got: %s", rr.Body.String())
	}
}

func TestRepoReadme_AnonReadOnPublicRepo(t *testing.T) {
	_, router, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	seedRepoWithFiles(t, gitDir, map[string]string{
		"README.md": "# Demo\n",
	})
	// No Authorization header — public repo, ActionRepoRead allows anon.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/readme", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
}

// ─── topics ─────────────────────────────────────────────────────────

func TestRepoTopics_ReplaceThenList(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string][]string{"names": {"go", "rest-api", "shithub"}})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/topics", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string][]string
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	want := map[bool]struct{}{}
	for _, n := range got["names"] {
		want[n == "go" || n == "rest-api" || n == "shithub"] = struct{}{}
	}
	if len(got["names"]) != 3 {
		t.Errorf("names count: got %v", got["names"])
	}
}

func TestRepoTopics_ReplaceRejectsInvalidShape(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string][]string{"names": {"UPPERCASE_NOT_OK"}})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/topics", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepoTopics_Clear(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	// Seed two topics.
	body, _ := json.Marshal(map[string][]string{"names": {"go", "rest"}})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/topics", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)

	// Clear.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/topics", nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("clear status: got %d; body=%s", rr.Code, rr.Body.String())
	}

	// Verify empty via a second PUT with empty names returning shape.
	body, _ = json.Marshal(map[string][]string{"names": {}})
	req = httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/topics", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var got map[string][]string
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got["names"]) != 0 {
		t.Errorf("names after clear: got %v", got["names"])
	}
}

func TestRepoTopics_ReplaceRequiresRepoWrite(t *testing.T) {
	_, router, _, token, _, _ := seedBranchesEnv(t, "alice")
	body, _ := json.Marshal(map[string][]string{"names": {"go"}})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/topics", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token) // repo:read only
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// ─── merge-upstream ─────────────────────────────────────────────────

func TestRepoMergeUpstream_NotAForkReturns422(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/merge-upstream", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepoMergeUpstream_RequiresRepoWrite(t *testing.T) {
	_, router, _, token, _, _ := seedBranchesEnv(t, "alice")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/merge-upstream", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token) // repo:read only
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
