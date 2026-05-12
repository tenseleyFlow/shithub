// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
)

type apiFollow struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	FollowedAt  string `json:"followed_at"`
}

func TestFollowers_PutThenProbeReturns204(t *testing.T) {
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	aliceToken := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeUserRead), string(pat.ScopeUserWrite))
	seedRepoCreatorUser(t, pool, "bob")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/user/following/bob", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("put status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	// Probe returns 204.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user/following/bob", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("probe status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFollowers_ProbeReturns404WhenNotFollowing(t *testing.T) {
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	aliceToken := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeUserRead))
	seedRepoCreatorUser(t, pool, "bob")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/following/bob", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFollowers_DeleteUnfollow(t *testing.T) {
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	aliceToken := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeUserRead), string(pat.ScopeUserWrite))
	seedRepoCreatorUser(t, pool, "bob")

	// Follow then unfollow.
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(m, "/api/v1/user/following/bob", nil)
		req.Header.Set("Authorization", "Bearer "+aliceToken)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("%s status: got %d, want 204; body=%s", m, rr.Code, rr.Body.String())
		}
	}

	// Probe after delete → 404.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/following/bob", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("post-delete probe: got %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFollowers_CannotFollowSelf(t *testing.T) {
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	aliceToken := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeUserWrite))

	req := httptest.NewRequest(http.MethodPut, "/api/v1/user/following/alice", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFollowers_FollowersList(t *testing.T) {
	// alice follows bob → list bob's followers, alice should be there.
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	aliceToken := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeUserRead), string(pat.ScopeUserWrite))
	seedRepoCreatorUser(t, pool, "bob")

	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/user/following/bob", nil)
	putReq.Header.Set("Authorization", "Bearer "+aliceToken)
	putRR := httptest.NewRecorder()
	router.ServeHTTP(putRR, putReq)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/bob/followers", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiFollow
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].Username != "alice" {
		t.Errorf("expected alice as bob's follower; got %+v", listed)
	}
}

func TestFollowers_FollowingList(t *testing.T) {
	// alice follows bob → list alice's `following`, bob should be there.
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	aliceToken := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeUserRead), string(pat.ScopeUserWrite))
	seedRepoCreatorUser(t, pool, "bob")

	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/user/following/bob", nil)
	putReq.Header.Set("Authorization", "Bearer "+aliceToken)
	router.ServeHTTP(httptest.NewRecorder(), putReq)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice/following", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiFollow
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].Username != "bob" {
		t.Errorf("expected bob in alice's following; got %+v", listed)
	}
}

func TestFollowers_UnknownUser404(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/ghost/followers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFollowers_PutRequiresUserWriteScope(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	readOnly := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))
	seedRepoCreatorUser(t, pool, "bob")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/user/following/bob", nil)
	req.Header.Set("Authorization", "Bearer "+readOnly)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFollowers_GetRequiresUserReadScope(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	wrong := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice/followers", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
