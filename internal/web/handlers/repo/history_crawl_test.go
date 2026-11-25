// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/web/handlers/repo/httpcache"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func (f *repoFixture) getHistory(viewer middleware.CurrentUser, target string) *httptest.ResponseRecorder {
	rw := httptest.NewRecorder()
	f.historyMuxWithViewer(viewer).ServeHTTP(rw, httptest.NewRequest(http.MethodGet, target, nil))
	return rw
}

// TestCommitsList_AnonymousFilteredHistoryRedirectsToLogin pins the
// crawler-trap fix: author/date filters and calendar paging are an
// unbounded URL space, so an anonymous request for one is sent to
// sign in before any git process starts. It must hold for branch
// refs and SHA-pinned refs alike.
func TestCommitsList_AnonymousFilteredHistoryRedirectsToLogin(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	headOID := f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	base := "/" + f.owner.Username + "/" + f.publicRepo.Name + "/commits/"

	for _, ref := range []string{"trunk", headOID} {
		for _, query := range []string{
			"author=test%40example.com",
			"until=2026-01-16",
			"since=2026-01-01",
			"calendar_month=2025-11",
			"author=x&calendar_month=2026-11&until=2026-11-02&page=3",
		} {
			target := base + ref + "?" + query
			rw := f.getHistory(anonymousViewer(), target)
			if rw.Code != http.StatusSeeOther {
				t.Fatalf("GET %s: status %d, want 303", target, rw.Code)
			}
			loc, err := url.Parse(rw.Header().Get("Location"))
			if err != nil || loc.Path != "/login" {
				t.Fatalf("GET %s: Location %q, want /login", target, rw.Header().Get("Location"))
			}
			if got := loc.Query().Get("next"); got != target {
				t.Errorf("GET %s: next = %q, want the original URL", target, got)
			}
		}
	}
}

// TestCommitsList_AnonymousUnfilteredAndPathHistoryStayPublic guards
// against over-blocking: plain history, paging and per-file history
// are ordinary browsing.
func TestCommitsList_AnonymousUnfilteredAndPathHistoryStayPublic(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	headOID := f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	base := "/" + f.owner.Username + "/" + f.publicRepo.Name + "/commits/"

	for _, target := range []string{
		base + "trunk",
		base + headOID,
		base + "trunk?page=1",
		base + "trunk?path=README.md",
		base + "trunk?author=&until=",
	} {
		if rw := f.getHistory(anonymousViewer(), target); rw.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", target, rw.Code)
		}
	}
}

func TestCommitsList_SignedInViewerCanFilter(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	target := "/" + f.owner.Username + "/" + f.publicRepo.Name + "/commits/trunk?author=test%40example.com&until=2026-12-31"

	rw := f.getHistory(viewerFor(f.owner), target)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
}

// TestCommitsList_FilterMenusOnlyForSignedInViewers checks the page
// stops advertising the filter links to anonymous visitors, so a
// crawler has nothing to follow into the trap.
func TestCommitsList_FilterMenusOnlyForSignedInViewers(t *testing.T) {
	t.Parallel()
	// Real templates: the assertions are about rendered markup.
	f := newRepoFixtureWithTemplates(t, os.DirFS("../../templates"), render.Options{Octicons: render.BuiltinOcticons()})
	f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	target := "/" + f.owner.Username + "/" + f.publicRepo.Name + "/commits/trunk"

	anon := f.getHistory(anonymousViewer(), target).Body.String()
	for _, leak := range []string{"shithub-commit-author-menu", "shithub-commit-calendar", "calendar_month=", "until=", "author="} {
		if strings.Contains(anon, leak) {
			t.Errorf("anonymous commits page still contains %q", leak)
		}
	}
	if !strings.Contains(anon, "Sign in to filter by author or date") {
		t.Errorf("anonymous commits page should offer sign-in in place of the filters")
	}

	authed := f.getHistory(viewerFor(f.owner), target).Body.String()
	for _, want := range []string{"shithub-commit-author-menu", "shithub-commit-calendar", "calendar_month="} {
		if !strings.Contains(authed, want) {
			t.Errorf("signed-in commits page is missing %q", want)
		}
	}
}

// TestCommitsList_SignedInRenderIsNeverCached is the regression test
// for a cross-viewer leak: the page cache key has no viewer in it,
// and the rendered page carries the viewer's nav and CSRF tokens, so
// a signed-in render must not be stored, served from the cache, or
// marked publicly cacheable.
func TestCommitsList_SignedInRenderIsNeverCached(t *testing.T) {
	t.Parallel()
	// Real templates: the assertions are about rendered markup.
	f := newRepoFixtureWithTemplates(t, os.DirFS("../../templates"), render.Options{Octicons: render.BuiltinOcticons()})
	cache := httpcache.NewPageCache(8, time.Minute)
	f.handlers.d.CommitsPageCache = cache
	headOID := f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	target := "/" + f.owner.Username + "/" + f.publicRepo.Name + "/commits/trunk"
	key := httpcache.PageKey{RepoID: f.publicRepo.ID, BranchOID: headOID, Page: 1}

	authed := f.getHistory(viewerFor(f.owner), target)
	if authed.Code != http.StatusOK {
		t.Fatalf("signed-in: status %d, want 200", authed.Code)
	}
	if _, ok := cache.Get(key); ok {
		t.Fatal("a signed-in render was stored in the shared page cache")
	}
	if got := authed.Header().Get("Cache-Control"); strings.Contains(got, "public") {
		t.Errorf("signed-in response has Cache-Control %q, must not be public", got)
	}
	if got := authed.Header().Get("ETag"); got != "" {
		t.Errorf("signed-in response has ETag %q; the tag is viewer-independent and must not be sent", got)
	}

	// An anonymous render fills the cache; a signed-in viewer must
	// still get their own render, not the anonymous bytes.
	anon := f.getHistory(anonymousViewer(), target)
	if _, ok := cache.Get(key); !ok {
		t.Fatal("anonymous render should populate the cache")
	}
	again := f.getHistory(viewerFor(f.owner), target)
	if again.Body.String() == anon.Body.String() {
		t.Error("signed-in viewer was served the cached anonymous page")
	}

	// A signed-in conditional request must not be answered 304 from
	// the viewer-independent tag.
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("If-None-Match", httpcache.ETag(f.publicRepo.ID, headOID, 1))
	rw := httptest.NewRecorder()
	f.historyMuxWithViewer(viewerFor(f.owner)).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("signed-in conditional request: status %d, want 200", rw.Code)
	}
}

func TestCommitsList_UnknownSHAIs404(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	target := "/" + f.owner.Username + "/" + f.publicRepo.Name + "/commits/" + strings.Repeat("0", 39) + "1"

	for _, viewer := range []middleware.CurrentUser{anonymousViewer(), viewerFor(f.owner)} {
		if rw := f.getHistory(viewer, target); rw.Code != http.StatusNotFound {
			t.Errorf("unknown SHA: status %d, want 404", rw.Code)
		}
	}
}

// TestCompareView_AnonymousSelectionRedirectsToLogin: a ref pair is
// a full diff render per URL, so only signed-in viewers get one. The
// landing page with no selection stays public.
func TestCompareView_AnonymousSelectionRedirectsToLogin(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	base := "/" + f.owner.Username + "/" + f.publicRepo.Name + "/compare"
	serve := func(viewer middleware.CurrentUser, target string) *httptest.ResponseRecorder {
		mux := chi.NewRouter()
		mux.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, withViewer(r, viewer))
			})
		})
		f.handlers.MountRefs(mux)
		rw := httptest.NewRecorder()
		mux.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, target, nil))
		return rw
	}

	for _, target := range []string{base + "/trunk...trunk", base + "/trunk..trunk", base + "/trunk"} {
		rw := serve(anonymousViewer(), target)
		if rw.Code != http.StatusSeeOther {
			t.Fatalf("anonymous GET %s: status %d, want 303", target, rw.Code)
		}
		loc, _ := url.Parse(rw.Header().Get("Location"))
		if loc == nil || loc.Path != "/login" || loc.Query().Get("next") != target {
			t.Errorf("anonymous GET %s: Location %q", target, rw.Header().Get("Location"))
		}
	}
	if rw := serve(anonymousViewer(), base); rw.Code != http.StatusOK {
		t.Errorf("anonymous GET %s: status %d, want 200", base, rw.Code)
	}
	if rw := serve(viewerFor(f.owner), base+"/trunk...trunk"); rw.Code != http.StatusOK {
		t.Errorf("signed-in GET compare selection: status %d, want 200", rw.Code)
	}
}
