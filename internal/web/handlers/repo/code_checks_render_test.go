// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/checks"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func TestCodeSurfacesRenderCommitCheckStatus(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	headOID := f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	detailsURL := "/alice/public-repo/actions/runs/7"
	f.createCompletedCodeCheck(t, f.publicRepo.ID, headOID, "success", detailsURL)

	historyMux := f.commitsListMux()
	for _, tt := range []struct {
		name string
		path string
		want string
	}{
		{
			name: "commit list",
			path: "/alice/public-repo/commits/trunk",
			want: "CHECK=" + headOID[:7] + ":success:" + detailsURL + ";",
		},
		{
			name: "commit detail",
			path: "/alice/public-repo/commit/" + headOID,
			want: "CHECK=success:" + detailsURL + ";",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			historyMux.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if resp.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200; body=%s", resp.Code, resp.Body.String())
			}
			if !strings.Contains(resp.Body.String(), tt.want) {
				t.Fatalf("missing %q in %s", tt.want, resp.Body.String())
			}
		})
	}

	refsMux := f.refsMuxWithViewer(anonymousViewer())
	resp := httptest.NewRecorder()
	refsMux.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/alice/public-repo/branches", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("branches status=%d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	if want := "BRANCH=trunk:success:" + detailsURL + ";"; !strings.Contains(resp.Body.String(), want) {
		t.Fatalf("missing branch status %q in %s", want, resp.Body.String())
	}

	commitRows := f.handlers.compareCommitRows(context.Background(), f.owner.Username, f.publicRepo.Name, f.publicRepo.ID, []repogit.Commit{
		{OID: headOID, ShortOID: headOID[:7]},
	})
	if len(commitRows) != 1 || !commitRows[0].Checks.Show || commitRows[0].Checks.StateClass != "success" || commitRows[0].Checks.Href != detailsURL {
		t.Fatalf("compareCommitRows = %+v, want success status linked to %s", commitRows, detailsURL)
	}
}

func TestCodeCheckStatusPrivateRepoKeepsSurrounding404(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	headOID := f.seedCommitsRepo(t, f.owner.Username, f.privateRepo.Name)
	f.createCompletedCodeCheck(t, f.privateRepo.ID, headOID, "failure", "/alice/private-repo/actions/runs/9")

	mux := f.historyMuxWithViewer(viewerFor(f.stranger))
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/alice/private-repo/commit/"+headOID, nil))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want privacy-preserving 404; body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "CHECK=") || strings.Contains(resp.Body.String(), "actions/runs/9") {
		t.Fatalf("private check status leaked in body: %s", resp.Body.String())
	}
}

func TestCodeCheckSummaryUsesLatestRunPerName(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	headOID := f.seedCommitsRepo(t, f.owner.Username, f.publicRepo.Name)
	f.createCompletedNamedCodeCheck(t, f.publicRepo.ID, headOID, "ci", "failure", "/alice/public-repo/actions/runs/1")
	f.createCompletedNamedCodeCheck(t, f.publicRepo.ID, headOID, "ci", "success", "/alice/public-repo/actions/runs/2")

	got := f.handlers.codeCommitCheckSummary(context.Background(), f.owner.Username, f.publicRepo.Name, f.publicRepo.ID, headOID)
	if !got.Show || got.StateClass != "success" || got.Href != "/alice/public-repo/actions/runs/2" {
		t.Fatalf("summary = %+v, want latest successful check linked to rerun", got)
	}
}

func (f *repoFixture) refsMuxWithViewer(viewer middleware.CurrentUser) *chi.Mux {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withViewer(r, viewer))
		})
	})
	f.handlers.MountRefs(mux)
	return mux
}

func (f *repoFixture) createCompletedCodeCheck(t *testing.T, repoID int64, headSHA, conclusion, detailsURL string) {
	t.Helper()
	f.createCompletedNamedCodeCheck(t, repoID, headSHA, "ci", conclusion, detailsURL)
}

func (f *repoFixture) createCompletedNamedCodeCheck(t *testing.T, repoID int64, headSHA, name, conclusion, detailsURL string) {
	t.Helper()
	_, err := checks.Create(context.Background(), checks.Deps{Pool: f.pool}, checks.CreateParams{
		RepoID:     repoID,
		HeadSHA:    headSHA,
		AppSlug:    "shithub-actions",
		Name:       name,
		Status:     "completed",
		Conclusion: conclusion,
		DetailsURL: detailsURL,
	})
	if err != nil {
		t.Fatalf("checks.Create: %v", err)
	}
}
