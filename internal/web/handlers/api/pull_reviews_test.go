// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	policydb "github.com/tenseleyFlow/shithub/internal/auth/policy/sqlc"
)

// addRepoCollaborator grants `role` on repoID to userID via the policy
// sqlc surface — bypassing the HTML collaborators page. Used by the
// review tests so a non-owner user can submit reviews (which requires
// RoleWrite per the policy.minRoleFor table).
func addRepoCollaborator(t *testing.T, pool *pgxpool.Pool, repoID, userID int64, role policydb.CollabRole) {
	t.Helper()
	if err := policydb.New().UpsertCollabRole(context.Background(), pool, policydb.UpsertCollabRoleParams{
		RepoID:        repoID,
		UserID:        userID,
		Role:          role,
		AddedByUserID: pgtype.Int8{Valid: false},
	}); err != nil {
		t.Fatalf("UpsertCollabRole: %v", err)
	}
}

type apiReview struct {
	ID        int64  `json:"id"`
	PullID    int64  `json:"pull_id"`
	AuthorID  int64  `json:"author_id"`
	State     string `json:"state"`
	Body      string `json:"body"`
	Dismissed bool   `json:"dismissed"`
}

type apiReviewComment struct {
	ID       int64  `json:"id"`
	PullID   int64  `json:"pull_id"`
	ReviewID int64  `json:"review_id"`
	AuthorID int64  `json:"author_id"`
	FilePath string `json:"file_path"`
	Body     string `json:"body"`
	Pending  bool   `json:"pending"`
	Resolved bool   `json:"resolved"`
}

type apiRequestedReviewer struct {
	ID            int64 `json:"id"`
	PullID        int64 `json:"pull_id"`
	UserID        int64 `json:"user_id"`
	RequestedByID int64 `json:"requested_by_id"`
}

// openPRForReviewTest seeds the standard PR (alice/demo, trunk←feature)
// so the rest of the tests can act on `pulls/1`. Bob is promoted to a
// `write` collaborator because the policy.ActionPullReview gate
// requires it — anyone-can-review on public repos is not the shithub
// posture (matches the HTML surface).
func openPRForReviewTest(t *testing.T) (router http.Handler, ownerToken, otherToken string, ownerID, otherID int64) {
	t.Helper()
	pool, router, ownerID, repoID, ownerToken, _ := seedPullsEnv(t, "alice")
	openPullFor(t, router, ownerToken, "alice", "demo")
	otherID = seedRepoCreatorUser(t, pool, "bob")
	otherToken = mintRunnerAPIPAT(t, pool, otherID, string(pat.ScopeRepoWrite))
	addRepoCollaborator(t, pool, repoID, otherID, policydb.CollabRoleWrite)
	return router, ownerToken, otherToken, ownerID, otherID
}

func TestReviews_SubmitComment(t *testing.T) {
	router, _, otherToken, _, _ := openPRForReviewTest(t)

	body, _ := json.Marshal(map[string]any{"event": "COMMENT", "body": "looks fine to me"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/reviews", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("submit: %d; body=%s", rr.Code, rr.Body.String())
	}
	var created apiReview
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.State != "comment" || created.Body != "looks fine to me" {
		t.Errorf("shape: %+v", created)
	}
}

func TestReviews_SubmitApprove(t *testing.T) {
	router, _, otherToken, _, _ := openPRForReviewTest(t)

	body, _ := json.Marshal(map[string]any{"event": "APPROVE"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/reviews", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("approve: %d; body=%s", rr.Code, rr.Body.String())
	}
	var created apiReview
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if created.State != "approve" {
		t.Errorf("state: %+v", created)
	}
}

func TestReviews_AuthorCannotApprove(t *testing.T) {
	router, ownerToken, _, _, _ := openPRForReviewTest(t)

	body, _ := json.Marshal(map[string]any{"event": "APPROVE"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/reviews", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestReviews_SubmitRejectsBadEvent(t *testing.T) {
	router, _, otherToken, _, _ := openPRForReviewTest(t)

	body, _ := json.Marshal(map[string]any{"event": "BANANA"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/reviews", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestReviews_List(t *testing.T) {
	router, _, otherToken, _, _ := openPRForReviewTest(t)
	for _, ev := range []string{"COMMENT", "APPROVE"} {
		body, _ := json.Marshal(map[string]any{"event": ev})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/reviews", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+otherToken)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d; body=%s", ev, rr.Code, rr.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1/reviews", nil)
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	var listed []apiReview
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Errorf("count: got %d, want 2; %+v", len(listed), listed)
	}
}

func TestReviewComments_AddAndList(t *testing.T) {
	router, _, otherToken, _, _ := openPRForReviewTest(t)

	body, _ := json.Marshal(map[string]any{
		"body":                "nit: unused import",
		"file_path":           "foo.txt",
		"side":                "right",
		"original_commit_sha": "abc123",
		"original_line":       1,
		"original_position":   1,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/comments", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("add comment: %d; body=%s", rr.Code, rr.Body.String())
	}
	var created apiReviewComment
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.FilePath != "foo.txt" || created.Body != "nit: unused import" {
		t.Errorf("shape: %+v", created)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1/comments", nil)
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	var listed []apiReviewComment
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Errorf("list: %+v", listed)
	}
}

func TestReviewComments_PendingAttachedOnSubmit(t *testing.T) {
	router, _, otherToken, _, _ := openPRForReviewTest(t)

	// Add a pending draft comment.
	body, _ := json.Marshal(map[string]any{
		"body":                "draft note",
		"file_path":           "foo.txt",
		"side":                "right",
		"original_commit_sha": "abc123",
		"original_line":       1,
		"original_position":   1,
		"pending":             true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/comments", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("add pending: %d; body=%s", rr.Code, rr.Body.String())
	}
	var draft apiReviewComment
	_ = json.Unmarshal(rr.Body.Bytes(), &draft)
	if !draft.Pending {
		t.Fatalf("expected pending=true; got %+v", draft)
	}

	// Submit a review — it should attach the pending draft.
	body, _ = json.Marshal(map[string]any{"event": "COMMENT", "body": "all my notes"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/reviews", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("submit: %d; body=%s", rr.Code, rr.Body.String())
	}
	var submitted apiReview
	_ = json.Unmarshal(rr.Body.Bytes(), &submitted)

	// List comments — the prior draft should now reference the review.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1/comments", nil)
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	var listed []apiReviewComment
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Fatalf("count: got %d, want 1; %+v", len(listed), listed)
	}
	if listed[0].ReviewID != submitted.ID {
		t.Errorf("draft not attached: %+v vs review %d", listed[0], submitted.ID)
	}
	if listed[0].Pending {
		t.Errorf("pending flag should be cleared after attach: %+v", listed[0])
	}
}

func TestReviewers_RequestAndDismiss(t *testing.T) {
	router, ownerToken, _, _, otherID := openPRForReviewTest(t)

	// Owner (alice) requests bob as a reviewer.
	body, _ := json.Marshal(map[string]any{"user_id": otherID})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/requested_reviewers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("request: %d; body=%s", rr.Code, rr.Body.String())
	}
	var created apiRequestedReviewer
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.UserID != otherID {
		t.Errorf("shape: %+v", created)
	}

	// List should show one pending request.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/pulls/1/requested_reviewers", nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	var listed []apiRequestedReviewer
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Errorf("count: got %d, want 1", len(listed))
	}

	// Same reviewer twice should 409.
	body, _ = json.Marshal(map[string]any{"user_id": otherID})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/requested_reviewers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("dup request: got %d, want 409; body=%s", rr.Code, rr.Body.String())
	}

	// Dismiss.
	body, _ = json.Marshal(map[string]any{"user_id": otherID})
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/pulls/1/requested_reviewers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("dismiss: %d; body=%s", rr.Code, rr.Body.String())
	}

	// Dismissing again returns 404 (no active request).
	body, _ = json.Marshal(map[string]any{"user_id": otherID})
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/pulls/1/requested_reviewers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("dismiss-twice: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestReviewers_RequestUnknownUserReturns422(t *testing.T) {
	router, ownerToken, _, _, _ := openPRForReviewTest(t)

	body, _ := json.Marshal(map[string]any{"username": "ghost"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/requested_reviewers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestReviews_RequiresScope(t *testing.T) {
	pool, router, ownerID, _, _, _ := seedPullsEnv(t, "alice")
	readOnly := mintRunnerAPIPAT(t, pool, ownerID, string(pat.ScopeRepoRead))
	_ = openPullFor(t, router, mintRunnerAPIPAT(t, pool, ownerID, string(pat.ScopeRepoWrite)), "alice", "demo")

	body, _ := json.Marshal(map[string]any{"event": "COMMENT"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/pulls/1/reviews", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+readOnly)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
