// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	gitops "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/repo/treecache"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// treeTemplatesFS is minimalTemplatesFS plus a repo/tree page that
// prints exactly the fields the real template renders from the
// last-commit column, so this test also pins the rendered output.
func treeTemplatesFS() fstest.MapFS {
	fsys := minimalTemplatesFS()
	fsys["repo/tree.html"] = &fstest.MapFile{Data: []byte(
		`{{ define "page" }}` +
			`{{ range .EntryRows }}ROW={{ .Entry.Name }}:{{ .LastFound }}:{{ .LastCommit.Subject }};{{ end }}` +
			`COUNT={{ .CommitCount }};` +
			`{{ end }}`)}
	return fsys
}

func newTreeFixture(t *testing.T) *repoFixture {
	t.Helper()
	return newRepoFixtureWithTemplates(t, treeTemplatesFS(), render.Options{})
}

func (f *repoFixture) treeMux() *chi.Mux {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withViewer(r, anonymousViewer()))
		})
	})
	f.handlers.MountCode(mux)
	return mux
}

// seedWideRepo materializes a bare repo on the fixture's RepoFS layout
// whose root tree has `entries` files plus a README, built over two
// commits so the last-commit column has something to resolve. History
// is scripted in a scratch worktree and cloned in bare, which is the
// cheapest way to get a multi-commit fixture.
func (f *repoFixture) seedWideRepo(t *testing.T, owner, name string, entries int) {
	t.Helper()
	gitDir, err := f.handlers.d.RepoFS.RepoPath(owner, name)
	if err != nil {
		t.Fatalf("RepoFS.RepoPath: %v", err)
	}
	src := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=Test Author", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test Author", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(src, rel), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	run(src, "init", "-q", "--initial-branch=trunk")
	write("README.md", "# demo\n")
	for i := 0; i < entries; i++ {
		write(fmt.Sprintf("f%04d.txt", i), "seed\n")
	}
	run(src, "add", "-A")
	run(src, "commit", "-q", "-m", "seed")
	write("f0000.txt", "touched\n")
	run(src, "add", "-A")
	run(src, "commit", "-q", "-m", "touch first entry")

	if out, err := exec.Command("git", "clone", "-q", "--bare", src, gitDir).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v: %s", err, out)
	}
}

// treeRepo creates a repo row + its bare git dir with `entries` files
// at the root and returns the row.
func (f *repoFixture) treeRepo(t *testing.T, name string, entries int) reposdb.Repo {
	t.Helper()
	row, err := reposdb.New().CreateRepo(context.Background(), f.pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: f.owner.ID, Valid: true},
		Name:          name,
		Visibility:    reposdb.RepoVisibilityPublic,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo %s: %v", name, err)
	}
	f.seedWideRepo(t, f.owner.Username, name, entries)
	return row
}

// getTree issues one anonymous GET of the repo's root tree and returns
// the number of git subprocesses it forked, plus the response.
func getTree(t *testing.T, mux *chi.Mux, owner, name string) (uint64, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/"+owner+"/"+name+"/tree/trunk", nil)
	rw := httptest.NewRecorder()
	before := gitops.ForkCount()
	mux.ServeHTTP(rw, req)
	forks := gitops.ForkCount() - before
	if rw.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	return forks, rw
}

// TestCodeTree_ForkCountIsIndependentOfEntryCount is the regression
// test for the availability campaign's Phase 3 CPU item: the code tab
// used to fork `git log -1` once per tree entry, so a crawler hitting
// a 100-entry directory cost ~100 git processes per anonymous request.
//
// NOT parallel: gitops.ForkCount is process-global, and Go guarantees
// sequential tests never overlap with parallel ones.
func TestCodeTree_ForkCountIsIndependentOfEntryCount(t *testing.T) {
	f := newTreeFixture(t)
	mux := f.treeMux()

	small := f.treeRepo(t, "narrow-repo", 5)
	large := f.treeRepo(t, "wide-repo", 80)

	smallForks, smallResp := getTree(t, mux, f.owner.Username, small.Name)
	largeForks, largeResp := getTree(t, mux, f.owner.Username, large.Name)
	t.Logf("git forks per cold tree render: 6 entries = %d, 81 entries = %d", smallForks, largeForks)

	if smallForks != largeForks {
		t.Errorf("forks scale with entry count: 6 entries = %d forks, 81 entries = %d forks",
			smallForks, largeForks)
	}
	// Belt and braces: the constant itself has to stay small. The
	// per-request reads are ListRefs, StatPath, LsTree, CommitAt, the
	// single last-commit walk, rev-list --count, the contributor log,
	// the recursive ls-tree, and the README reads.
	const forkCeiling = 16
	if largeForks > forkCeiling {
		t.Errorf("cold tree render forked git %d times, ceiling is %d", largeForks, forkCeiling)
	}

	// Output parity: every entry still gets its last-commit cell, and
	// the entry touched by the second commit reports that commit.
	for _, body := range []string{smallResp.Body.String(), largeResp.Body.String()} {
		if strings.Contains(body, ":false:") {
			t.Errorf("some rows lost their last-commit cell: %s", body)
		}
		if !strings.Contains(body, "ROW=f0000.txt:true:touch first entry;") {
			t.Errorf("f0000.txt should report the second commit: %s", body)
		}
		if !strings.Contains(body, "ROW=README.md:true:seed;") {
			t.Errorf("README.md should report the seed commit: %s", body)
		}
		if !strings.Contains(body, "COUNT=2;") {
			t.Errorf("commit count should be 2: %s", body)
		}
	}
}

// TestCodeTree_WarmCacheDropsGitForks pins the second half of the fix:
// once the OID-keyed cache is warm, the repeat views a crawler
// generates skip the last-commit walk, rev-list --count, the
// contributor log, and the recursive ls-tree entirely.
func TestCodeTree_WarmCacheDropsGitForks(t *testing.T) {
	f := newTreeFixture(t)
	f.handlers.d.TreeCache = treecache.New(treecache.DefaultCapacity, treecache.DefaultTTL)
	mux := f.treeMux()
	row := f.treeRepo(t, "warm-repo", 40)

	coldForks, coldResp := getTree(t, mux, f.owner.Username, row.Name)
	warmForks, warmResp := getTree(t, mux, f.owner.Username, row.Name)
	t.Logf("git forks per tree render: cold = %d, warm = %d", coldForks, warmForks)

	if warmForks >= coldForks {
		t.Errorf("warm render forked %d times vs %d cold; the cache is not being used",
			warmForks, coldForks)
	}
	if got := f.handlers.d.TreeCache.Stats(); got.Hits == 0 {
		t.Errorf("tree cache recorded no hits: %+v", got)
	}
	if warmResp.Body.String() != coldResp.Body.String() {
		t.Errorf("warm body differs from cold body:\ncold=%s\nwarm=%s",
			coldResp.Body.String(), warmResp.Body.String())
	}
}

// TestCodeTree_CacheMissesAfterAPush proves the invalidation contract:
// the cache key carries the rendered commit OID, so a push produces a
// new key rather than serving the pre-push listing.
func TestCodeTree_CacheMissesAfterAPush(t *testing.T) {
	f := newTreeFixture(t)
	f.handlers.d.TreeCache = treecache.New(treecache.DefaultCapacity, treecache.DefaultTTL)
	mux := f.treeMux()
	row := f.treeRepo(t, "pushed-repo", 4)

	_, before := getTree(t, mux, f.owner.Username, row.Name)
	if !strings.Contains(before.Body.String(), "ROW=f0000.txt:true:touch first entry;") {
		t.Fatalf("unexpected pre-push body: %s", before.Body.String())
	}

	// Land a third commit straight onto the bare repo's branch.
	gitDir, err := f.handlers.d.RepoFS.RepoPath(f.owner.Username, row.Name)
	if err != nil {
		t.Fatalf("RepoFS.RepoPath: %v", err)
	}
	pushCommit(t, gitDir, "f0000.txt", "pushed\n", "third commit")

	_, after := getTree(t, mux, f.owner.Username, row.Name)
	if !strings.Contains(after.Body.String(), "ROW=f0000.txt:true:third commit;") {
		t.Errorf("post-push render served a stale last-commit column: %s", after.Body.String())
	}
	if !strings.Contains(after.Body.String(), "COUNT=3;") {
		t.Errorf("post-push commit count should be 3: %s", after.Body.String())
	}
}

// pushCommit writes one blob onto trunk in a bare repo using the same
// plumbing sequence the web editor uses, and moves the ref.
func pushCommit(t *testing.T, gitDir, path, body, message string) {
	t.Helper()
	indexFile := filepath.Join(t.TempDir(), "index")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", gitDir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=Test Author", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test Author", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_AUTHOR_DATE=2026-02-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-02-01T00:00:00Z",
			"GIT_INDEX_FILE="+indexFile,
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	head := git("rev-parse", "refs/heads/trunk")
	git("read-tree", head)
	blob := gitStdin(t, gitDir, body, "hash-object", "-w", "--stdin")
	git("update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	tree := git("write-tree")
	commit := git("commit-tree", tree, "-p", head, "-m", message)
	git("update-ref", "refs/heads/trunk", commit, head)
}

func gitStdin(t *testing.T, gitDir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", gitDir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
