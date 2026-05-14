// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// forkCheck wires the chi router and calls /{owner}/{repo}/fork/check-name
// the way the live server does — required because the handler reads
// owner/repo via chi.URLParam.
func (f *repoFixture) forkCheck(t *testing.T, ownerName, repoName string, viewer middleware.CurrentUser, q url.Values) forkCheckResult {
	t.Helper()
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withViewer(r, viewer))
		})
	})
	f.handlers.MountFork(mux)

	target := "/" + ownerName + "/" + repoName + "/fork/check-name"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	var got forkCheckResult
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body (status=%d, body=%s): %v", rw.Code, rw.Body.String(), err)
	}
	return got
}

func TestForkCheckName_Available(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	q := url.Values{
		"target_owner": []string{"user:" + strconv.FormatInt(f.stranger.ID, 10)},
		"target_name":  []string{"fresh-fork"},
	}
	got := f.forkCheck(t, f.owner.Username, f.publicRepo.Name, viewerFor(f.stranger), q)
	if got.Status != "available" {
		t.Errorf("status = %q, want available (message=%q)", got.Status, got.Message)
	}
}

func TestForkCheckName_TakenUnderTargetOwner(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	// Stranger already owns a repo named "collision" — forking the
	// public repo and renaming it to that should report taken.
	_, err := reposdb.New().CreateRepo(context.Background(), f.pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: f.stranger.ID, Valid: true},
		Name:          "collision",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("seed collision: %v", err)
	}
	q := url.Values{
		"target_owner": []string{"user:" + strconv.FormatInt(f.stranger.ID, 10)},
		"target_name":  []string{"collision"},
	}
	got := f.forkCheck(t, f.owner.Username, f.publicRepo.Name, viewerFor(f.stranger), q)
	if got.Status != "taken" {
		t.Errorf("status = %q, want taken (message=%q)", got.Status, got.Message)
	}
}

func TestForkCheckName_InvalidShape(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	q := url.Values{
		"target_owner": []string{"user:" + strconv.FormatInt(f.stranger.ID, 10)},
		"target_name":  []string{"..bad-dotted"},
	}
	got := f.forkCheck(t, f.owner.Username, f.publicRepo.Name, viewerFor(f.stranger), q)
	if got.Status != "invalid" {
		t.Errorf("status = %q, want invalid (message=%q)", got.Status, got.Message)
	}
}

func TestForkCheckName_ForbiddenOwner(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	// Stranger asks to fork into the OWNER's namespace — they don't
	// own that user account, so the check must surface forbidden.
	q := url.Values{
		"target_owner": []string{"user:" + strconv.FormatInt(f.owner.ID, 10)},
		"target_name":  []string{"whatever"},
	}
	got := f.forkCheck(t, f.owner.Username, f.publicRepo.Name, viewerFor(f.stranger), q)
	if got.Status != "forbidden" {
		t.Errorf("status = %q, want forbidden (message=%q)", got.Status, got.Message)
	}
}

func TestForkCheckName_SelfForkSameName(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	// Owner attempts to fork their own public-repo back into their
	// own namespace with the same name — the modal should warn rather
	// than let the POST fail with ErrSelfForkSameName.
	q := url.Values{
		"target_owner": []string{"user:" + strconv.FormatInt(f.owner.ID, 10)},
		"target_name":  []string{f.publicRepo.Name},
	}
	got := f.forkCheck(t, f.owner.Username, f.publicRepo.Name, viewerFor(f.owner), q)
	if got.Status != "taken" {
		t.Errorf("status = %q, want taken/self-fork hint (message=%q)", got.Status, got.Message)
	}
}
