// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	gitops "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/repo/httpcache"
)

// seedCommitsRepo materializes a bare git repo on the fixture's
// RepoFS layout so the commits handler can actually run `git log`
// against it. Reuses the production InitialCommit plumbing — same
// path the create-repo flow exercises in prod.
func (f *repoFixture) seedCommitsRepo(t *testing.T, owner, name string) string {
	t.Helper()
	gitDir, err := f.handlers.d.RepoFS.RepoPath(owner, name)
	if err != nil {
		t.Fatalf("RepoFS.RepoPath: %v", err)
	}
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=trunk", gitDir).CombinedOutput(); err != nil {
		t.Fatalf("git init %q: %v: %s", gitDir, err, out)
	}
	if _, err := (gitops.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Test",
		AuthorEmail: "test@example.com",
		Branch:      "trunk",
		When:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Files: []gitops.FileEntry{
			{Path: "README.md", Body: []byte("# demo\n")},
		},
	}).Build(context.Background()); err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	oid, err := gitops.ResolveRefOID(context.Background(), gitDir, "trunk")
	if err != nil {
		t.Fatalf("ResolveRefOID: %v", err)
	}
	return oid
}

// commitsListMux mounts MountHistory + an OptionalUser-style
// middleware that injects an anonymous viewer for the request.
// The real RequestID/RealIP middleware isn't needed for these
// tests because the handler only reads the user from context.
func (f *repoFixture) commitsListMux() *chi.Mux {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withViewer(r, anonymousViewer()))
		})
	})
	f.handlers.MountHistory(mux)
	return mux
}

func TestCommitsList_SetsETagAndCacheHeaders_OnColdRequest(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	headOID := f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	mux := f.commitsListMux()

	req := httptest.NewRequest(http.MethodGet, "/"+f.owner.Username+"/"+f.publicRepo.Name+"/commits/trunk", nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	wantETag := httpcache.ETag(f.publicRepo.ID, headOID, 1)
	if got := rw.Header().Get("ETag"); got != wantETag {
		t.Errorf("ETag header = %q, want %q", got, wantETag)
	}
	if got := rw.Header().Get("Cache-Control"); got != "public, max-age=60, must-revalidate" {
		t.Errorf("Cache-Control = %q, want public, max-age=60, must-revalidate", got)
	}
	if got := rw.Header().Get("Vary"); !strings.Contains(got, "Cookie") {
		t.Errorf("Vary = %q, want to contain Cookie", got)
	}
}

func TestCommitsList_Returns304_WhenIfNoneMatchMatches(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	headOID := f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	mux := f.commitsListMux()

	etag := httpcache.ETag(f.publicRepo.ID, headOID, 1)
	req := httptest.NewRequest(http.MethodGet, "/"+f.owner.Username+"/"+f.publicRepo.Name+"/commits/trunk", nil)
	req.Header.Set("If-None-Match", etag)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusNotModified {
		t.Fatalf("status=%d, want 304; body=%s", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("ETag"); got != etag {
		t.Errorf("ETag header on 304 = %q, want %q", got, etag)
	}
	if rw.Body.Len() != 0 {
		t.Errorf("304 body must be empty; got %q", rw.Body.String())
	}
}

func TestCommitsList_Renders200_WhenIfNoneMatchMismatches(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	_ = f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	mux := f.commitsListMux()

	req := httptest.NewRequest(http.MethodGet, "/"+f.owner.Username+"/"+f.publicRepo.Name+"/commits/trunk", nil)
	req.Header.Set("If-None-Match", `"stale-tag-from-another-deploy"`)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 on stale tag; body=%s", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("ETag"); got == "" {
		t.Errorf("ETag still must be set on 200")
	}
}

func TestCommitsList_FilteredRequest_NoETagOrCacheControl(t *testing.T) {
	t.Parallel()
	// Filtered views (path / author / since / until) bypass the
	// cache because (repo_id, branch_oid, page) is the same across
	// filter variants — caching against that key would serve
	// wrong content. Confirm by querying with a path filter and
	// asserting the cache headers are absent.
	f := newRepoFixture(t)
	_ = f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	mux := f.commitsListMux()

	req := httptest.NewRequest(http.MethodGet, "/"+f.owner.Username+"/"+f.publicRepo.Name+"/commits/trunk?path=README.md", nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("ETag"); got != "" {
		t.Errorf("ETag must not be set on filtered request; got %q", got)
	}
	if got := rw.Header().Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control must not be set on filtered request; got %q", got)
	}
}
