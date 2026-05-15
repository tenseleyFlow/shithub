// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// TestSettingsGeneral_IsTemplateRoundtrip confirms the PRO-EXT01-06pre
// template-repo toggle flips repos.is_template via the general settings
// form. Free + public — no gate yet (PRO-EXT01-06 adds it).
func TestSettingsGeneral_IsTemplateRoundtrip(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	mux := f.generalSettingsMux(f.owner.ID, f.owner.Username)

	// Default: is_template is false.
	assertRepoIsTemplate(t, f, f.publicRepo.ID, false)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/public-repo/settings/general", url.Values{
		"description": {"a template"},
		"is_template": {"on"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST is_template=on status=%d body=%s", resp.Code, resp.Body.String())
	}
	assertRepoIsTemplate(t, f, f.publicRepo.ID, true)

	// Toggle off again — omit the field entirely (HTML checkbox semantics).
	resp = httptest.NewRecorder()
	req = newFormRequest(http.MethodPost, "/alice/public-repo/settings/general", url.Values{
		"description": {"a template"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST is_template=off status=%d body=%s", resp.Code, resp.Body.String())
	}
	assertRepoIsTemplate(t, f, f.publicRepo.ID, false)
}

func (f *repoFixture) generalSettingsMux(userID int64, username string) http.Handler {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: userID, Username: username}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	f.handlers.MountSettingsGeneral(mux)
	return mux
}

func assertRepoIsTemplate(t *testing.T, f *repoFixture, repoID int64, want bool) {
	t.Helper()
	var got bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT is_template FROM repos WHERE id = $1`, repoID,
	).Scan(&got); err != nil {
		t.Fatalf("read is_template: %v", err)
	}
	if got != want {
		t.Errorf("repos.is_template: got %v, want %v", got, want)
	}
}
