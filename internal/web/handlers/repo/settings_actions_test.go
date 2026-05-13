// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func TestSettingsActionsRepoSecretCRUDDoesNotRenderPlaintext(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	f.handlers.d.SecretBox = testSecretBox(t)
	mux := f.actionsSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/public-repo/settings/secrets/actions", url.Values{
		"name":  {"DEPLOY_KEY"},
		"value": {"hunter2"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST secret status=%d body=%s", resp.Code, resp.Body.String())
	}

	var ciphertext []byte
	if err := f.pool.QueryRow(context.Background(),
		`SELECT ciphertext FROM workflow_secrets WHERE repo_id = $1 AND name = $2`,
		f.publicRepo.ID, "DEPLOY_KEY").Scan(&ciphertext); err != nil {
		t.Fatalf("query ciphertext: %v", err)
	}
	if strings.Contains(string(ciphertext), "hunter2") {
		t.Fatal("plaintext appeared in workflow_secrets.ciphertext")
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/settings/secrets/actions", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET secret list status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "SECRET=DEPLOY_KEY;") {
		t.Fatalf("secret name missing from list: %s", body)
	}
	if strings.Contains(body, "hunter2") {
		t.Fatalf("secret plaintext leaked in list body: %s", body)
	}

	resp = httptest.NewRecorder()
	req = newFormRequest(http.MethodPost, "/alice/public-repo/settings/secrets/actions/DEPLOY_KEY/delete", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("DELETE secret status=%d body=%s", resp.Code, resp.Body.String())
	}
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM workflow_secrets WHERE repo_id = $1 AND name = $2`,
		f.publicRepo.ID, "DEPLOY_KEY").Scan(&count); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if count != 0 {
		t.Fatalf("secret row count=%d, want 0", count)
	}
}

func TestSettingsActionsRepoVariableCRUDRendersValue(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	mux := f.actionsSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/public-repo/settings/variables/actions", url.Values{
		"name":  {"IMAGE_TAG"},
		"value": {"2026.05"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST variable status=%d body=%s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/settings/variables/actions", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET variable list status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); !strings.Contains(got, "VAR=IMAGE_TAG:2026.05;") {
		t.Fatalf("variable missing from list: %s", got)
	}

	resp = httptest.NewRecorder()
	req = newFormRequest(http.MethodPost, "/alice/public-repo/settings/variables/actions/IMAGE_TAG/delete", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("DELETE variable status=%d body=%s", resp.Code, resp.Body.String())
	}
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM actions_variables WHERE repo_id = $1 AND name = $2`,
		f.publicRepo.ID, "IMAGE_TAG").Scan(&count); err != nil {
		t.Fatalf("count variables: %v", err)
	}
	if count != 0 {
		t.Fatalf("variable row count=%d, want 0", count)
	}
}

func TestSettingsActionsPolicyCRUD(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	mux := f.actionsSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/settings/actions", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET policy status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); !strings.Contains(got, "POLICY=inherit:inherit:true:true:50;") {
		t.Fatalf("default policy missing: %s", got)
	}

	resp = httptest.NewRecorder()
	req = newFormRequest(http.MethodPost, "/alice/public-repo/settings/actions", url.Values{
		"actions_enabled":              {"disabled"},
		"require_pr_approval":          {"false"},
		"max_repo_queued_runs":         {"3"},
		"max_repo_concurrent_jobs":     {"2"},
		"max_owner_concurrent_jobs":    {"5"},
		"actor_trigger_limit_per_hour": {"7"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST policy status=%d body=%s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/settings/actions", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET saved policy status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); !strings.Contains(got, "POLICY=disabled:false:false:false:3;") {
		t.Fatalf("saved policy missing: %s", got)
	}
}

func (f *repoFixture) actionsSettingsMux(userID int64, username string) http.Handler {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: userID, Username: username}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	f.handlers.MountSettingsActions(mux)
	return mux
}

func newFormRequest(method, target string, form url.Values) *http.Request {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func testSecretBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	box, err := secretbox.FromBytes(key)
	if err != nil {
		t.Fatalf("FromBytes: %v", err)
	}
	return box
}
