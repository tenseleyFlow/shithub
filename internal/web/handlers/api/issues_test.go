// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// apiUser mirrors the server's userEnvelope minus the optional
// avatar/html_url fields the tests don't assert on.
type apiUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

type apiIssue struct {
	ID          int64      `json:"id"`
	Number      int64      `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	StateReason string     `json:"state_reason"`
	Locked      bool       `json:"locked"`
	LockReason  string     `json:"lock_reason"`
	AuthorID    int64      `json:"author_id"`
	User        *apiUser   `json:"user"`
	HTMLURL     string     `json:"html_url"`
	Labels      []apiLabel `json:"labels"`
	Assignees   []apiUser  `json:"assignees"`
	CreatedAt   string     `json:"created_at"`
	UpdatedAt   string     `json:"updated_at"`
	ClosedAt    string     `json:"closed_at"`
}

type apiComment struct {
	ID        int64    `json:"id"`
	IssueID   int64    `json:"issue_id"`
	AuthorID  int64    `json:"author_id"`
	User      *apiUser `json:"user"`
	Body      string   `json:"body"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

// seedIssuesEnv stands up a one-shot test environment: pool + router +
// owner user + a public repo (`alice/demo` by default) + a PAT scoped
// to repo:write. Tests that need a second actor (Bob) build him with
// seedRepoCreatorUser against the returned pool.
func seedIssuesEnv(t *testing.T, ownerUsername string) (pool *pgxpool.Pool, router http.Handler, userID, repoID int64, token string) {
	t.Helper()
	pool = dbtest.NewTestDB(t)
	router, rfs := newReposAPIRouter(t, pool)
	userID = seedRepoCreatorUser(t, pool, ownerUsername)
	token = mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	res, err := repos.Create(context.Background(), repos.Deps{
		Pool:    pool,
		RepoFS:  rfs,
		Audit:   audit.NewRecorder(),
		Limiter: throttle.NewLimiter(),
	}, repos.Params{
		ActorUserID:   userID,
		OwnerUserID:   userID,
		OwnerUsername: ownerUsername,
		Name:          "demo",
		Description:   "demo repo",
		Visibility:    "public",
	})
	if err != nil {
		t.Fatalf("repos.Create: %v", err)
	}
	return pool, router, userID, res.Repo.ID, token
}

func TestIssues_CreateAndGet(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "first bug", "body": "kaboom"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var created apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Number != 1 || created.Title != "first bug" || created.State != "open" {
		t.Errorf("shape: %+v", created)
	}
	// B7: response must carry the user-facing URL so CLI clients can
	// surface it on success ("Created issue X #N (URL)"). The test
	// harness sets BaseURL to https://shithub.test.
	if !strings.HasSuffix(created.HTMLURL, "/alice/demo/issues/1") {
		t.Errorf("html_url: got %q, want suffix /alice/demo/issues/1", created.HTMLURL)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var fetched apiIssue
	_ = json.Unmarshal(rr.Body.Bytes(), &fetched)
	if !strings.HasSuffix(fetched.HTMLURL, "/alice/demo/issues/1") {
		t.Errorf("GET html_url: got %q, want suffix /alice/demo/issues/1", fetched.HTMLURL)
	}
}

func TestIssues_CreateRejectsEmptyTitle(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "", "body": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_CreateRequiresRepoWriteScope(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	readOnly := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	body, _ := json.Marshal(map[string]any{"title": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+readOnly)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// TestIssues_CreateWithLabelsAssigneesMilestone covers the S63/B3
// extension: POST /issues honoring labels[], assignees[], milestone.
// Pre-S63 these fields were silently dropped on the server side and
// gh-compat CLI flags (`shithub issue create --label bug --assignee
// alice --milestone 1`) had to do a follow-up PATCH to take effect.
func TestIssues_CreateWithLabelsAssigneesMilestone(t *testing.T) {
	pool, router, _, repoID, token := seedIssuesEnv(t, "alice")
	ctx := context.Background()

	// Seed via the REST surface so we exercise the same paths the CLI
	// hits. Names avoid the system-seeded defaults ("bug", "enhancement",
	// etc.) — the labelCreate endpoint 409s on those.
	for _, name := range []string{"triaged", "p1"} {
		lbody, _ := json.Marshal(map[string]any{"name": name, "color": "ff0000"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/labels", bytes.NewReader(lbody))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed label %q: %d %s", name, rr.Code, rr.Body.String())
		}
	}

	// Milestone seeded directly — there's no milestone REST surface
	// yet and we just need a row whose id we can pass through.
	m, err := issuesdb.New().CreateMilestone(ctx, pool, issuesdb.CreateMilestoneParams{
		RepoID: repoID, Title: "v1", DueOn: pgtype.Timestamptz{},
	})
	if err != nil {
		t.Fatalf("seed milestone: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"title":     "with attachments",
		"body":      "boom",
		"labels":    []string{"bug", "p1"},
		"assignees": []string{"alice"},
		"milestone": m.ID,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var created apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(created.Labels); got != 2 {
		t.Fatalf("labels: got %d, want 2; raw=%s", got, rr.Body.String())
	}
	names := map[string]bool{}
	for _, l := range created.Labels {
		names[l.Name] = true
	}
	if !names["bug"] || !names["p1"] {
		t.Errorf("label names: got %+v", created.Labels)
	}

	// C20a: assignees must also surface in the issue response
	// envelope. Pre-D1, presentIssue dropped them — the CLI then
	// rendered "no assignees" on issues that actually had them.
	if len(created.Assignees) != 1 {
		t.Errorf("response.assignees: got %d, want 1; raw=%s", len(created.Assignees), rr.Body.String())
	} else if created.Assignees[0].Login != "alice" {
		t.Errorf("response.assignees[0].login: got %q, want alice", created.Assignees[0].Login)
	}

	// Server-side verify assignee + milestone landed on the issue row
	// (defense in depth: response envelope says one thing, DB says
	// another == bug).
	assignees, err := issuesdb.New().ListIssueAssignees(ctx, pool, created.ID)
	if err != nil {
		t.Fatalf("ListIssueAssignees: %v", err)
	}
	if len(assignees) != 1 {
		t.Errorf("assignees: got %d, want 1", len(assignees))
	}
	fresh, err := issuesdb.New().GetIssueByID(ctx, pool, created.ID)
	if err != nil {
		t.Fatalf("GetIssueByID: %v", err)
	}
	if !fresh.MilestoneID.Valid || fresh.MilestoneID.Int64 != m.ID {
		t.Errorf("milestone: got %+v, want %d", fresh.MilestoneID, m.ID)
	}
}

// TestIssues_CreateUnknownLabelRejected confirms the up-front
// resolution path: a typo'd label name fails the request with 422 and
// no issue row is written. Prevents the half-created-issue footgun.
func TestIssues_CreateUnknownLabelRejected(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{
		"title":  "x",
		"labels": []string{"does-not-exist"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}

	// And confirm no issue was inserted — the list should be empty.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	var listed []apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("expected no issues after rejected create, got %d", len(listed))
	}
}

// TestIssues_ListLabelsFilter covers C-audit C8: passing `labels=`
// must either filter on the named labels (gh-compat AND semantic) or
// reject with 422 when any name doesn't exist on the repo. Pre-fix
// the filter was silently dropped and the full unfiltered list came
// back — the kind of false-negative-on-filter bug that bites scripts.
func TestIssues_ListLabelsFilter(t *testing.T) {
	pool, router, _, repoID, token := seedIssuesEnv(t, "alice")
	// Create two issues; we'll attach "triaged" to only the first.
	for _, title := range []string{"triaged one", "untagged one"} {
		body, _ := json.Marshal(map[string]any{"title": title})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed issue: %d", rr.Code)
		}
	}
	// Seed a label and attach to issue #1 via PATCH.
	lbody, _ := json.Marshal(map[string]any{"name": "triaged", "color": "ff0000"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/labels", bytes.NewReader(lbody))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed label: %d %s", rr.Code, rr.Body.String())
	}
	patch, _ := json.Marshal(map[string]any{"labels": []string{"triaged"}})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/1", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("attach label: %d %s", rr.Code, rr.Body.String())
	}

	// Happy path: filter matches the single issue carrying the label.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues?labels=triaged", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: %d %s", rr.Code, rr.Body.String())
	}
	var listed []apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].Title != "triaged one" {
		t.Errorf("filtered list shape: %+v", listed)
	}

	// C8 regression: unknown label name must 422, not silently return
	// the full unfiltered list. Pre-fix this returned 2 rows.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues?labels=totally-fake", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown label status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unknown label") {
		t.Errorf("error should mention unknown label; got %s", rr.Body.String())
	}

	// Compound: real + fake → still 422 (any unknown rejects).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues?labels=triaged,totally-fake", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("compound filter with unknown: got %d, want 422", rr.Code)
	}

	_ = pool
	_ = repoID
}

func TestIssues_ListFiltersByState(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	// Create two issues, close the second directly via sqlc.
	for i, title := range []string{"open one", "closed one"} {
		body, _ := json.Marshal(map[string]any{"title": title})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %d: %d", i, rr.Code)
		}
	}
	q := issuesdb.New()
	issue, err := q.GetIssueByNumber(context.Background(), pool, issuesdb.GetIssueByNumberParams{
		RepoID: repoID, Number: 2,
	})
	if err != nil {
		t.Fatalf("GetIssueByNumber: %v", err)
	}
	if err := q.SetIssueState(context.Background(), pool, issuesdb.SetIssueStateParams{
		ID:             issue.ID,
		State:          issuesdb.IssueStateClosed,
		StateReason:    issuesdb.NullIssueStateReason{Valid: false},
		ClosedByUserID: pgtype.Int8{Int64: userID, Valid: true},
	}); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}

	for _, tc := range []struct {
		state string
		want  int
	}{
		{"open", 1},
		{"closed", 1},
		{"all", 2},
		{"", 2},
	} {
		url := "/api/v1/repos/alice/demo/issues"
		if tc.state != "" {
			url += "?state=" + tc.state
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("state=%q: %d; body=%s", tc.state, rr.Code, rr.Body.String())
		}
		var listed []apiIssue
		if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(listed) != tc.want {
			t.Errorf("state=%q count: got %d, want %d", tc.state, len(listed), tc.want)
		}
	}
}

func TestIssues_PatchTitleBodyState(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"title": "old", "body": "old body"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	patch, _ := json.Marshal(map[string]any{"title": "new", "body": "new body", "state": "closed", "state_reason": "completed"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/1", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Title != "new" || updated.Body != "new body" {
		t.Errorf("title/body: %+v", updated)
	}
	if updated.State != "closed" || updated.StateReason != "completed" {
		t.Errorf("state: %+v", updated)
	}
}

func TestIssues_PatchTitleByOtherForbidden(t *testing.T) {
	pool, router, _, _, tokenAlice := seedIssuesEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"title": "alice's bug"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	patch, _ := json.Marshal(map[string]any{"title": "hijack"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/1", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_CommentsCRUD(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"title": "needs feedback"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue seed: %d", rr.Code)
	}

	body, _ = json.Marshal(map[string]any{"body": "lgtm"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues/1/comments", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment create: %d; body=%s", rr.Code, rr.Body.String())
	}
	var created apiComment
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Body != "lgtm" || created.IssueID == 0 {
		t.Errorf("shape: %+v", created)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1/comments", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: %d", rr.Code)
	}
	var listed []apiComment
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Errorf("list: %+v", listed)
	}

	patch, _ := json.Marshal(map[string]any{"body": "lgtm — second look"})
	req = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/repos/alice/demo/issues/comments/%d", created.ID), bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/repos/alice/demo/issues/comments/%d", created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_CommentEditByNonAuthorForbidden(t *testing.T) {
	pool, router, _, _, tokenAlice := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "thread"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue seed: %d", rr.Code)
	}

	body, _ = json.Marshal(map[string]any{"body": "alice's comment"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues/1/comments", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment seed: %d", rr.Code)
	}
	var created apiComment
	_ = json.Unmarshal(rr.Body.Bytes(), &created)

	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	patch, _ := json.Marshal(map[string]any{"body": "hijacked"})
	req = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/repos/alice/demo/issues/comments/%d", created.ID), bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_LockUnlock(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "spicy"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/issues/1/lock", strings.NewReader(`{"lock_reason":"off-topic"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("lock status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d", rr.Code)
	}
	var fetched apiIssue
	_ = json.Unmarshal(rr.Body.Bytes(), &fetched)
	if !fetched.Locked || fetched.LockReason != "off-topic" {
		t.Errorf("locked state: %+v", fetched)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/issues/1/lock", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("unlock status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

// TestIssues_LockedRejectsCommentEvenFromLocker covers C-audit C19:
// pre-fix, the locker (who is a collaborator) bypassed their own
// lock and could keep commenting. "Lock that doesn't lock" was the
// UX cliff. Strict semantic: any comment to a locked issue gets a
// 423 until the issue is explicitly unlocked.
func TestIssues_LockedRejectsCommentEvenFromLocker(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	// Create + lock as the same actor.
	cbody, _ := json.Marshal(map[string]any{"title": "spicy"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(cbody))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}
	req = httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/issues/1/lock", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("lock: %d", rr.Code)
	}

	// Comment as the locker — must be rejected 423 (regression for C19).
	commentBody, _ := json.Marshal(map[string]any{"body": "post-lock by locker"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues/1/comments", bytes.NewReader(commentBody))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusLocked {
		t.Fatalf("locked comment: got %d, want 423 Locked; body=%s", rr.Code, rr.Body.String())
	}

	// After unlock, comments work again.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/issues/1/lock", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("unlock: %d", rr.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues/1/comments", bytes.NewReader(commentBody))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Errorf("post-unlock comment: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
}

// TestIssues_UserEnvelope pins the S60 audit-finding A12 fix: every
// issue + comment response carries a nested `user: {id, login, type}`
// envelope alongside the legacy `author_id` so gh-compat clients (the
// shithub-cli, which renders `ghost` when user is missing) work
// directly without a separate /users/{id} round-trip.
func TestIssues_UserEnvelope(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "envelope-check", "body": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status: got %d, body=%s", rr.Code, rr.Body.String())
	}
	var created apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.User == nil {
		t.Fatalf("issue.user envelope missing; body=%s", rr.Body.String())
	}
	if created.User.Login != "alice" || created.User.Type != "User" {
		t.Errorf("user envelope shape: %+v", created.User)
	}
	if created.User.ID != created.AuthorID {
		t.Errorf("user.id (%d) != author_id (%d)", created.User.ID, created.AuthorID)
	}

	// Single GET path also exercises the resolveUserEnvelope code path
	// (vs the create path which uses the authenticated caller's id).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status: %d; body=%s", rr.Code, rr.Body.String())
	}
	var fetched apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fetched.User == nil || fetched.User.Login != "alice" {
		t.Errorf("fetched user envelope: %+v", fetched.User)
	}

	// Posting a comment exercises presentComment + envelope on the
	// /comments POST path.
	cbody, _ := json.Marshal(map[string]any{"body": "first reply"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues/1/comments", bytes.NewReader(cbody))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment status: %d; body=%s", rr.Code, rr.Body.String())
	}
	var c apiComment
	if err := json.Unmarshal(rr.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	if c.User == nil || c.User.Login != "alice" {
		t.Errorf("comment user envelope: %+v", c.User)
	}
}
