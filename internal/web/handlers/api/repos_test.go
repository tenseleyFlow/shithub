// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
)

// apiRepoOwner mirrors repoOwnerEnvelope.
type apiRepoOwner struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// apiRepoLicense mirrors repoLicenseEnvelope.
type apiRepoLicense struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	SPDXID string `json:"spdx_id"`
	URL    string `json:"url"`
	NodeID string `json:"node_id"`
}

type apiRepo struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	// I7c (audit-I18): flat owner_login/owner_type fields dropped;
	// read owner.login + owner.type.
	Owner         *apiRepoOwner   `json:"owner"`
	Description   string          `json:"description"`
	Homepage      string          `json:"homepage"`
	Visibility    string          `json:"visibility"`
	Private       bool            `json:"private"`
	HTMLURL       string          `json:"html_url"`
	DefaultBranch string          `json:"default_branch"`
	Fork          bool            `json:"fork"`
	Archived      bool            `json:"archived"`
	IsTemplate    bool            `json:"is_template"`
	HasIssues     bool            `json:"has_issues"`
	HasPulls      bool            `json:"has_pulls"`
	StarCount     int64           `json:"star_count"`
	WatcherCount  int64           `json:"watcher_count"`
	ForkCount     int64           `json:"fork_count"`
	Topics        []string        `json:"topics"`
	License       *apiRepoLicense `json:"license"`
	Language      string          `json:"language"`
	Size          int64           `json:"size"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	PushedAt      string          `json:"pushed_at"`

	// I7a (audit-I11): gh-compat field expansion.
	NodeID                    string                  `json:"node_id"`
	Parent                    *apiRepo                `json:"parent"`
	Permissions               *apiRepoPermissions     `json:"permissions"`
	SubscribersCount          int64                   `json:"subscribers_count"`
	NetworkCount              int64                   `json:"network_count"`
	AllowSquashMerge          bool                    `json:"allow_squash_merge"`
	AllowRebaseMerge          bool                    `json:"allow_rebase_merge"`
	AllowMergeCommit          bool                    `json:"allow_merge_commit"`
	AllowAutoMerge            bool                    `json:"allow_auto_merge"`
	AllowUpdateBranch         bool                    `json:"allow_update_branch"`
	DeleteBranchOnMerge       bool                    `json:"delete_branch_on_merge"`
	UseSquashPRTitleAsDefault bool                    `json:"use_squash_pr_title_as_default"`
	WebCommitSignoffRequired  bool                    `json:"web_commit_signoff_required"`
	SquashMergeCommitTitle    string                  `json:"squash_merge_commit_title"`
	SquashMergeCommitMessage  string                  `json:"squash_merge_commit_message"`
	MergeCommitTitle          string                  `json:"merge_commit_title"`
	MergeCommitMessage        string                  `json:"merge_commit_message"`
	MirrorURL                 *string                 `json:"mirror_url"`
	TemplateRepository        *apiRepo                `json:"template_repository"`
	SecurityAndAnalysis       *apiSecurityAndAnalysis `json:"security_and_analysis"`
}

type apiRepoPermissions struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Triage   bool `json:"triage"`
	Pull     bool `json:"pull"`
}

type apiSecurityAndAnalysis struct {
	SecretScanning               apiSecurityFeature `json:"secret_scanning"`
	SecretScanningPushProtection apiSecurityFeature `json:"secret_scanning_push_protection"`
	DependabotSecurityUpdates    apiSecurityFeature `json:"dependabot_security_updates"`
}

type apiSecurityFeature struct {
	Status string `json:"status"`
}

// newReposAPIRouter builds an API router with the repo-create stack
// wired in: Audit, Throttle, and a per-test RepoFS rooted at t.TempDir.
// ShithubdPath is left empty so hook installation is a no-op (matches
// the repos.Create test fixtures).
func newReposAPIRouter(t *testing.T, pool *pgxpool.Pool) (http.Handler, *storage.RepoFS) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewRepoFS: %v", err)
	}
	h, err := apih.New(apih.Deps{
		Pool:        pool,
		Logger:      logger,
		RepoFS:      rfs,
		Audit:       audit.NewRecorder(),
		Throttle:    throttle.NewLimiter(),
		RateLimiter: ratelimit.New(pool),
		BaseURL:     "https://shithub.test",
		APILimit: apilimit.Config{
			AuthedPerHour: 5000,
			AnonPerHour:   60,
			Logger:        logger,
		},
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	return r, rfs
}

func seedRepoCreatorUser(t *testing.T, pool *pgxpool.Pool, username string) (userID int64) {
	t.Helper()
	ctx := context.Background()
	q := usersdb.New()
	user, err := q.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     username,
		DisplayName:  strings.ToUpper(username[:1]) + username[1:],
		PasswordHash: runnerAPIFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	em, err := q.CreateUserEmail(ctx, pool, usersdb.CreateUserEmailParams{
		UserID:    user.ID,
		Email:     username + "@example.test",
		IsPrimary: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	if err := q.MarkUserEmailVerified(ctx, pool, em.ID); err != nil {
		t.Fatalf("MarkUserEmailVerified: %v", err)
	}
	if err := q.LinkUserPrimaryEmail(ctx, pool, usersdb.LinkUserPrimaryEmailParams{
		ID:             user.ID,
		PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	}); err != nil {
		t.Fatalf("LinkUserPrimaryEmail: %v", err)
	}
	if err := q.MarkUserEmailPrimaryVerified(ctx, pool, user.ID); err != nil {
		t.Fatalf("MarkUserEmailPrimaryVerified: %v", err)
	}
	return user.ID
}

func TestRepos_CreatePersonalAndGet(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{
		"name":        "demo",
		"description": "first cut",
		"visibility":  "public",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var created apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v; body=%s", err, rr.Body.String())
	}
	if created.Name != "demo" || created.Owner == nil || created.Owner.Login != "alice" || created.Owner.Type != "User" {
		t.Errorf("create payload: %+v", created)
	}
	if created.Visibility != "public" || created.Private {
		t.Errorf("visibility: got %+v, want public/public", created)
	}
	if created.DefaultBranch != "trunk" {
		t.Errorf("default_branch: got %q, want trunk", created.DefaultBranch)
	}
	// S62 audit B14: nested owner envelope populated alongside legacy
	// flat fields. CLI's `repo view --json owner` rendered {login:"",
	// type:""} before this; pin the envelope so a future regression
	// surfaces in CI instead of in a user's terminal.
	if created.Owner == nil || created.Owner.Login != "alice" || created.Owner.Type != "User" {
		t.Errorf("owner envelope: %+v", created.Owner)
	}
	// HTMLURL is BaseURL + "/" + full_name. The test router uses
	// "https://shithub.test" so we can assert the prefix.
	if !strings.HasPrefix(created.HTMLURL, "https://shithub.test/alice/demo") {
		t.Errorf("html_url: got %q, want https://shithub.test/alice/demo*", created.HTMLURL)
	}
	// pushed_at is best-effort (= updated_at); just confirm it is set.
	if created.PushedAt == "" {
		t.Error("pushed_at should be populated")
	}

	// GET single repo.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var fetched apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if fetched.Owner == nil || fetched.Owner.Login != "alice" {
		t.Errorf("fetched owner envelope: %+v", fetched.Owner)
	}
	if fetched.HTMLURL == "" {
		t.Error("fetched html_url should be populated")
	}
}

// TestRepos_CreateWithLicensePopulatesLicenseName pins F8: the repo
// response's `license.name` must contain the SPDX title (e.g. "MIT
// License"), not an empty string. Pre-G13 only `license.key` was
// populated; gh-compat clients displaying `license.name` saw blanks.
func TestRepos_CreateWithLicensePopulatesLicenseName(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{
		"name":             "licensed",
		"visibility":       "public",
		"init_readme":      true,
		"license_template": "MIT",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.License == nil {
		t.Fatal("license envelope missing on create response")
	}
	// I7a (audit-I11): gh-compat splits the SPDX casing across two
	// fields — `key` is lowercase (the URL-safe id), `spdx_id` is the
	// canonical SPDX casing. Pre-I7a, `key` carried the canonical
	// casing; that shape mismatched gh's documented surface.
	if created.License.Key != "mit" {
		t.Errorf("license.key: got %q want %q (gh-compat: lowercase)", created.License.Key, "mit")
	}
	if created.License.SPDXID != "MIT" {
		t.Errorf("license.spdx_id: got %q want %q", created.License.SPDXID, "MIT")
	}
	if created.License.Name != "MIT License" {
		t.Errorf("license.name: got %q want %q", created.License.Name, "MIT License")
	}

	// Verify GET also populates the field.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/licensed", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var fetched apiRepo
	_ = json.Unmarshal(rr.Body.Bytes(), &fetched)
	if fetched.License == nil || fetched.License.Name != "MIT License" {
		t.Errorf("GET license.name: %+v", fetched.License)
	}
}

func TestRepos_CreateRejectsBadName(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "BAD..NAME", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

// TestRepos_CreateRejectsHostileNames pins H3 (H12): byte-exact name
// validation. Pre-fix the API handler called repos.NormalizeName which
// silently trimmed whitespace and lowercased — so `"  Demo  "` was
// saved as `"demo"` and the user had no idea their input was rewritten.
func TestRepos_CreateRejectsHostileNames(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	for _, name := range []string{
		"  leading-space",
		"trailing-space  ",
		"DEMO",
		"De-Mo",
		" demo",
		"demo ",
	} {
		body, _ := json.Marshal(map[string]any{"name": name, "visibility": "public"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("name=%q: code=%d want 422; body=%s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestRepos_CreateRejectsDuplicate(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	mk := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}
	if rr := mk(); rr.Code != http.StatusCreated {
		t.Fatalf("first create: %d", rr.Code)
	}
	rr := mk()
	if rr.Code != http.StatusConflict {
		t.Fatalf("dup create: got %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_CreateRequiresRepoWriteScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_ListAuthedUserSeesPrivate(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	for _, spec := range []struct {
		name, vis string
	}{
		{"demo-public", "public"},
		{"demo-private", "private"},
	} {
		body, _ := json.Marshal(map[string]any{"name": spec.name, "visibility": spec.vis})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d; body=%s", spec.name, rr.Code, rr.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/repos", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want 200", rr.Code)
	}
	var listed []apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("count: got %d, want 2; %+v", len(listed), listed)
	}
}

func TestRepos_ListOtherUserPublicOnly(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	aliceID := seedRepoCreatorUser(t, pool, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenAlice := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeRepoWrite))
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead))

	// Alice creates one public + one private.
	for _, spec := range []struct{ name, vis string }{
		{"public-one", "public"},
		{"private-one", "private"},
	} {
		body, _ := json.Marshal(map[string]any{"name": spec.name, "visibility": spec.vis})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tokenAlice)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d", spec.name, rr.Code)
		}
	}

	// Bob lists alice's repos — should see only the public one.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice/repos", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	var listed []apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "public-one" {
		t.Fatalf("public-only filter failed: %+v", listed)
	}
}

func TestRepos_GetPrivateHidesFromOthers(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	aliceID := seedRepoCreatorUser(t, pool, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenAlice := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeRepoWrite))
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead))

	body, _ := json.Marshal(map[string]any{"name": "secret", "visibility": "private"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d; %s", rr.Code, rr.Body.String())
	}

	// Bob asks for the private repo directly — must 404 (existence leak).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/secret", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_PatchDescriptionAndVisibility(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public", "description": "old"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	patch, _ := json.Marshal(map[string]any{"description": "new", "visibility": "private"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if updated.Description != "new" {
		t.Errorf("description: got %q, want %q", updated.Description, "new")
	}
	if updated.Visibility != "private" || !updated.Private {
		t.Errorf("visibility: got %+v", updated)
	}
}

// TestRepos_PatchRejectsRename is the C7 regression: PATCH /repos
// with a {"name": "..."} field used to be silently dropped (Go's
// default JSON decoder discards unknown fields). The CLI took the
// 200 + unchanged repo response as success, rendered "Renamed to
// <OLD name>", and (worse) overwrote the local git origin to point
// at the renamed-to URL — see CX2 on the CLI side. Until rename is
// implemented server-side, refuse with a clear 422.
// C7: PATCH with a valid new name renames the repo via lifecycle.Rename
// and returns the updated repo object so `shithub repo rename` can
// confirm the change actually happened.
func TestRepos_PatchRenamesRepo(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	patch, _ := json.Marshal(map[string]any{"name": "renamed"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "renamed" || got.FullName != "alice/renamed" {
		t.Errorf("rename not reflected: %+v", got)
	}

	// Follow-up GET on the new name must work; the old slug must now
	// 404 (redirect rows handle the HTML side; REST GET goes 404).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/renamed", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("GET new name: %d; body=%s", rr.Code, rr.Body.String())
	}
}

// TestRepos_PatchHomepagePersists is the E7 regression: pre-fix the
// PATCH handler silently dropped the homepage field (the column didn't
// even exist). Migration 0116 adds the column; the round-trip is now:
// POST repo → PATCH homepage=… → GET surfaces the persisted value.
func TestRepos_PatchHomepagePersists(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}
	var created apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Homepage != "" {
		t.Errorf("fresh repo: homepage = %q, want empty", created.Homepage)
	}

	// PATCH homepage. Response must reflect the new value AND a follow-up
	// GET must surface it — the audit caught this gap on the GET side.
	patch, _ := json.Marshal(map[string]any{"homepage": "https://example.com"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status: %d; body=%s", rr.Code, rr.Body.String())
	}
	var patched apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if patched.Homepage != "https://example.com" {
		t.Errorf("patch response: homepage = %q, want https://example.com", patched.Homepage)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d", rr.Code)
	}
	var got apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.Homepage != "https://example.com" {
		t.Errorf("get response: homepage = %q, want https://example.com", got.Homepage)
	}
}

// TestRepos_PatchHomepagePreservedAcrossUnrelatedPatch confirms the
// "default to existing value when not specified" branch: patching only
// description must not clear a previously-set homepage.
func TestRepos_PatchHomepagePreservedAcrossUnrelatedPatch(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	for _, p := range []map[string]any{
		{"homepage": "https://example.com"},
		{"description": "now described"},
	} {
		raw, _ := json.Marshal(p)
		req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		rr = httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("patch %v: %d; body=%s", p, rr.Code, rr.Body.String())
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var got apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Homepage != "https://example.com" {
		t.Errorf("homepage cleared by description patch: got %q", got.Homepage)
	}
	if got.Description != "now described" {
		t.Errorf("description: got %q", got.Description)
	}
}

// TestRepos_PatchDefaultBranchUnknownBranchIs422 is the E28 regression:
// PATCH /repos {"default_branch":"nothing"} used to fall through to a
// 200 with no DB change. We now 422 when the branch isn't in the repo's
// ref list, matching gh and avoiding the false-success footgun.
func TestRepos_PatchDefaultBranchUnknownBranchIs422(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	patch, _ := json.Marshal(map[string]any{"default_branch": "nothing"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("not found")) {
		t.Errorf("error body should mention not found; got %s", rr.Body.String())
	}

	// Defense in depth: confirm the DB row was not touched.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status: %d", rr.Code)
	}
	var got apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DefaultBranch != "trunk" {
		t.Errorf("default_branch leaked update: got %q, want trunk", got.DefaultBranch)
	}
}

// TestRepos_PatchDefaultBranchEmptyIs422 catches the trivial bad input.
func TestRepos_PatchDefaultBranchEmptyIs422(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	patch, _ := json.Marshal(map[string]any{"default_branch": "   "})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

// C7: empty / too-long / illegal names get 422.
func TestRepos_PatchRenameRejectsInvalidName(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	cases := []string{"", "foo/bar", strings.Repeat("x", 200)}
	for _, name := range cases {
		patch, _ := json.Marshal(map[string]any{"name": name})
		req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(patch))
		req.Header.Set("Authorization", "Bearer "+token)
		rr = httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("name=%q: got %d, want 422; body=%s", name, rr.Code, rr.Body.String())
		}
	}
}

// C7: same-name rename returns 422 — gh treats this as a validation
// error rather than a no-op so clients don't print a false success.
func TestRepos_PatchRenameSameNameIs422(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	patch, _ := json.Marshal(map[string]any{"name": "demo"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("same-name: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

// C7: a rename to a name already used by another repo owned by the
// same user returns 409.
func TestRepos_PatchRenameTakenIs409(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	for _, name := range []string{"first", "second"} {
		body, _ := json.Marshal(map[string]any{"name": name, "visibility": "public"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d", name, rr.Code)
		}
	}

	patch, _ := json.Marshal(map[string]any{"name": "second"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/first", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_PatchRejectsNonOwner(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	aliceID := seedRepoCreatorUser(t, pool, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenAlice := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeRepoWrite))
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	patch, _ := json.Marshal(map[string]any{"description": "evil"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_DeleteSoftDeletes(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "throwaway", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/throwaway", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	// Subsequent GET 404s.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/throwaway", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("post-delete get: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_DeleteOnlyOwners(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	aliceID := seedRepoCreatorUser(t, pool, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenAlice := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeRepoWrite))
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-user delete: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// E15: `repo list --visibility nonsense` previously returned every
// repo. Now invalid values 422; valid values narrow correctly.
func TestRepos_ListVisibilityStrict(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	// One public + one private repo so visibility filtering actually
	// has something to narrow against.
	for _, vis := range []string{"public", "private"} {
		body, _ := json.Marshal(map[string]any{"name": "demo-" + vis, "visibility": vis})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d", vis, rr.Code)
		}
	}

	for _, tc := range []struct {
		filter   string
		wantCode int
		wantLen  int
	}{
		{"", 200, 2},
		{"public", 200, 1},
		{"private", 200, 1},
		{"internal", 200, 0},
		{"nonsense", 422, 0},
		{"PUBLIC", 200, 1}, // case-insensitive accepted
	} {
		url := "/api/v1/user/repos"
		if tc.filter != "" {
			url += "?visibility=" + tc.filter
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("visibility=%q: code=%d want %d; body=%s", tc.filter, rr.Code, tc.wantCode, rr.Body.String())
			continue
		}
		if tc.wantCode != 200 {
			continue
		}
		var rows []apiRepo
		_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		if len(rows) != tc.wantLen {
			t.Errorf("visibility=%q: got %d rows, want %d", tc.filter, len(rows), tc.wantLen)
		}
	}
}

// archiveRepoViaAPI helps the archive-trap tests below. Fails the test
// only after confirming the repo's archived flag flipped server-side.
func archiveRepoViaAPI(t *testing.T, router http.Handler, token, owner, name string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"archived": true})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/repos/"+owner+"/"+name, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("archive %s/%s: %d; body=%s", owner, name, rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+owner+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get after archive: %d", rr.Code)
	}
	var got apiRepo
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if !got.Archived {
		t.Fatalf("archive did not stick; repo=%+v", got)
	}
}

// E9: archive used to be a one-way trap because the policy gate blocked
// every write on archived repos — including the unarchive itself. After
// the fix, PATCH `archived=false` on an archived repo succeeds.
func TestRepos_PatchUnarchiveEscapeValve(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}
	archiveRepoViaAPI(t, router, token, "alice", "demo")

	body, _ = json.Marshal(map[string]any{"archived": false})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unarchive: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var got apiRepo
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Archived {
		t.Errorf("unarchive did not stick; repo=%+v", got)
	}
}

// E18: archived-repo writes used to 404 "repo not found", making
// archived indistinguishable from deleted. Now the deny code propagates
// through to a 403 with an "archived" message — clients can recover.
func TestRepos_PatchOnArchivedRepoIs403(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}
	archiveRepoViaAPI(t, router, token, "alice", "demo")

	body, _ = json.Marshal(map[string]any{"description": "new desc on archived"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("archived patch: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "archived") {
		t.Errorf("403 body should mention archived; got %s", rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("archived GET: got %d, want 200", rr.Code)
	}
}

// TestRepos_GetCarriesGHCompatExpansion pins audit-I11: GET on a single
// repo emits the full gh-compat field surface — node_id, permissions
// bundle for the authed actor, network/subscribers counts, the merge-
// strategy toggle constellation, the security-and-analysis stub
// envelope, and explicit `null` for the deferred mirror_url +
// template_repository fields. Ported gh scripts rely on these.
func TestRepos_GetCarriesGHCompatExpansion(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{
		"name":       "expansion",
		"visibility": "public",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/expansion", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}

	var got apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// NodeID: opaque but must be set + base64-shaped.
	if got.NodeID == "" {
		t.Error("node_id missing")
	}

	// Permissions: alice is owner → all true.
	if got.Permissions == nil {
		t.Fatal("permissions missing on owner GET")
	}
	if !got.Permissions.Admin || !got.Permissions.Push || !got.Permissions.Pull {
		t.Errorf("owner permissions: %+v (want admin+push+pull true)", got.Permissions)
	}

	// Network + subscribers counts present.
	if got.NetworkCount < 0 || got.SubscribersCount < 0 {
		t.Errorf("counts: network=%d subscribers=%d", got.NetworkCount, got.SubscribersCount)
	}

	// Merge-strategy toggles: real fields default to true (squash + merge
	// + rebase all on at create time); deferred toggles must be false.
	if got.AllowAutoMerge || got.AllowUpdateBranch || got.UseSquashPRTitleAsDefault || got.WebCommitSignoffRequired {
		t.Errorf("deferred toggles should default false: %+v", got)
	}
	// Merge-commit format constants pinned.
	if got.SquashMergeCommitTitle != "COMMIT_OR_PR_TITLE" {
		t.Errorf("squash_merge_commit_title: got %q", got.SquashMergeCommitTitle)
	}
	if got.MergeCommitMessage != "PR_TITLE" {
		t.Errorf("merge_commit_message: got %q", got.MergeCommitMessage)
	}

	// MirrorURL + TemplateRepository explicit null.
	if got.MirrorURL != nil {
		t.Errorf("mirror_url should be null, got %v", got.MirrorURL)
	}
	if got.TemplateRepository != nil {
		t.Errorf("template_repository should be null, got %v", got.TemplateRepository)
	}

	// Security + analysis stub: all features disabled.
	if got.SecurityAndAnalysis == nil {
		t.Fatal("security_and_analysis envelope missing")
	}
	if got.SecurityAndAnalysis.SecretScanning.Status != "disabled" {
		t.Errorf("secret_scanning: %+v", got.SecurityAndAnalysis.SecretScanning)
	}
	if got.SecurityAndAnalysis.DependabotSecurityUpdates.Status != "disabled" {
		t.Errorf("dependabot_security_updates: %+v", got.SecurityAndAnalysis.DependabotSecurityUpdates)
	}

	// Parent: non-fork repo → nil parent.
	if got.Parent != nil {
		t.Errorf("non-fork parent: got %+v, want nil", got.Parent)
	}
}
