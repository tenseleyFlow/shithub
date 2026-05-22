// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/orgs"
)

type apiOrg struct {
	ID                    int64  `json:"id"`
	Slug                  string `json:"slug"`
	Login                 string `json:"login"`
	DisplayName           string `json:"display_name"`
	Description           string `json:"description"`
	Plan                  string `json:"plan"`
	AllowMemberRepoCreate bool   `json:"allow_member_repo_create"`
}

type apiMembership struct {
	OrgID       int64  `json:"org_id"`
	Slug        string `json:"slug"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Website     string `json:"website"`
	AvatarURL   string `json:"avatar_url"`
	CreatedAt   string `json:"created_at"`
	Role        string `json:"role"`
}

type apiOrgMember struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// seedOrgFor builds an org owned by the supplied user. Uses the same
// orgs.Create path the HTML surface exercises so the principals row +
// owner membership land coherently.
func seedOrgFor(t *testing.T, pool *pgxpool.Pool, ownerID int64, slug, displayName string) int64 {
	t.Helper()
	row, err := orgs.Create(context.Background(), orgs.Deps{Pool: pool}, orgs.CreateParams{
		Slug:            slug,
		DisplayName:     displayName,
		Description:     displayName + " org",
		CreatedByUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	return row.ID
}

func TestOrgs_UserOrgsListSelf(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))
	seedOrgFor(t, pool, userID, "acme", "Acme")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/orgs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiMembership
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 || listed[0].Slug != "acme" {
		t.Errorf("shape: %+v", listed)
	}
	if listed[0].Role != "owner" {
		t.Errorf("role: got %q, want owner", listed[0].Role)
	}
	// F47: detail fields must be populated; pre-fix everything past
	// slug + role came back zero-valued and the CLI's `--json`
	// exporter surfaced empty strings.
	if listed[0].Description != "Acme org" {
		t.Errorf("description: got %q, want %q", listed[0].Description, "Acme org")
	}
	if listed[0].CreatedAt == "" {
		t.Errorf("created_at: got empty, want RFC3339 timestamp")
	}
	if listed[0].DisplayName != "Acme" {
		t.Errorf("display_name: got %q, want %q", listed[0].DisplayName, "Acme")
	}
}

func TestOrgs_UserOrgsListPublic(t *testing.T) {
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeUserRead))
	seedOrgFor(t, pool, aliceID, "acme", "Acme")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice/orgs", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiMembership
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 || listed[0].Slug != "acme" {
		t.Errorf("shape: %+v", listed)
	}
}

func TestOrgs_UserOrgsListUnknownUserReturns404(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/ghost/orgs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestOrgs_OrgGet(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))
	seedOrgFor(t, pool, userID, "acme", "Acme")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/orgs/acme", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var org apiOrg
	if err := json.Unmarshal(rr.Body.Bytes(), &org); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if org.Slug != "acme" || org.Login != "acme" || org.DisplayName != "Acme" {
		t.Errorf("shape: %+v", org)
	}
}

func TestOrgs_OrgGetUnknownReturns404(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/orgs/no-such-org", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestOrgs_OrgMembersList(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))
	seedOrgFor(t, pool, userID, "acme", "Acme")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/orgs/acme/members", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var members []apiOrgMember
	if err := json.Unmarshal(rr.Body.Bytes(), &members); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(members) != 1 || members[0].Username != "alice" || members[0].Role != "owner" {
		t.Errorf("shape: %+v", members)
	}
}

func TestOrgs_RequiresUserReadScope(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	repoOnly := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/orgs", nil)
	req.Header.Set("Authorization", "Bearer "+repoOnly)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestOrgs_UserOrgsListRequiresAuth(t *testing.T) {
	_, router, _, _, _ := seedIssuesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/orgs", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}
