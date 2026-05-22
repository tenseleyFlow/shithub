// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	ID          int64           `json:"id"`
	Number      int64           `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	StateReason string          `json:"state_reason"`
	Locked      bool            `json:"locked"`
	LockReason  string          `json:"lock_reason"`
	AuthorID    int64           `json:"author_id"`
	User        *apiUser        `json:"user"`
	HTMLURL     string          `json:"html_url"`
	Labels      []apiLabel      `json:"labels"`
	Assignees   []apiUser       `json:"assignees"`
	Milestone   *apiMilestoneIE `json:"milestone"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	ClosedAt    string          `json:"closed_at"`
}

// apiMilestoneIE mirrors the server's milestoneIssueEnvelope — the
// trimmed shape that surfaces on issue responses (no issue counts).
type apiMilestoneIE struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
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

// TestIssues_CreateRejectsNullByteInBody pins H3: posting a body
// containing `\x00` previously truncated silently at the null (Postgres
// TEXT columns can't hold null bytes — the pgx driver dropped them).
// Now the orchestrator rejects with a typed 422.
func TestIssues_CreateRejectsNullByteInBody(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{
		"title": "nullbyte",
		"body":  "before\x00after",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("null byte")) {
		t.Errorf("error should mention null byte: %s", rr.Body.String())
	}
}

// TestIssues_CreateRejectsNullByteInTitle is the symmetric H3 case for
// the title field.
func TestIssues_CreateRejectsNullByteInTitle(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{
		"title": "abc\x00def",
		"body":  "ok",
	})
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

	// E3 regression: the response must surface the milestone envelope,
	// not drop the field entirely. Pre-fix the response had no
	// `milestone` key at all and the CLI's `--json milestone` exporter
	// returned null even when the row stored a milestone id.
	if created.Milestone == nil {
		t.Errorf("response.milestone: got nil, want envelope (E3)")
	} else if created.Milestone.ID != m.ID || created.Milestone.Title != "v1" {
		t.Errorf("response.milestone: %+v", created.Milestone)
	}
}

// TestIssues_ResponseMilestoneIsNullWhenAbsent is the other half of E3:
// an issue with no milestone surfaces `milestone: null`, not an absent
// key. Matches gh — clients can distinguish "no milestone" from "field
// removed from the schema" only when the key is always present.
func TestIssues_ResponseMilestoneIsNullWhenAbsent(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "no milestone", "body": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: %d; body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"milestone":null`)) {
		t.Errorf("missing `\"milestone\":null` in response; raw=%s", rr.Body.String())
	}
}

// TestIssues_ResponseAlwaysIncludesLabelsKey covers E27: pre-fix the
// `labels` field carried `omitempty` so an issue with no labels
// silently dropped the key entirely. gh-compat clients (and the CLI
// `--json labels` exporter) expect the key to always be present as
// `[]`. The check is on the raw JSON because both behaviours decode
// to a zero-length slice on the Go side.
func TestIssues_ResponseAlwaysIncludesLabelsKey(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "no labels", "body": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"labels":[]`)) {
		t.Errorf("create response missing `\"labels\":[]` key; raw=%s", rr.Body.String())
	}

	// And the same shape on a GET against the freshly-created issue —
	// the audit's repro was a GET (`shithub api .../issues/1`) not the
	// POST response.
	var created apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	getReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/repos/alice/demo/issues/"+strconv.FormatInt(created.Number, 10), nil)
	getReq.Header.Set("Authorization", "Bearer "+token)
	getRR := httptest.NewRecorder()
	router.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get status: %d", getRR.Code)
	}
	if !bytes.Contains(getRR.Body.Bytes(), []byte(`"labels":[]`)) {
		t.Errorf("get response missing `\"labels\":[]` key; raw=%s", getRR.Body.String())
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
	if _, err := q.SetIssueState(context.Background(), pool, issuesdb.SetIssueStateParams{
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

// TestIssues_PatchAlreadyClosedWithReasonReturns422 pins H14: when the
// issue is already in target state AND the caller passed state_reason,
// surface a typed 422 so the user sees their reason-change intent was
// lost. Bare `state` change on already-matching state (no state_reason)
// keeps gh-compat idempotent success.
func TestIssues_PatchAlreadyClosedWithReasonReturns422(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"title": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	// First close.
	close1, _ := json.Marshal(map[string]any{"state": "closed", "state_reason": "completed"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/1", bytes.NewReader(close1))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first close: %d %s", rr.Code, rr.Body.String())
	}

	// Re-close with a different state_reason — should now 422.
	close2, _ := json.Marshal(map[string]any{"state": "closed", "state_reason": "not_planned"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/1", bytes.NewReader(close2))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("re-close with reason: got %d want 422; body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("already closed")) {
		t.Errorf("body should mention already closed: %s", rr.Body.String())
	}

	// Bare re-close (no state_reason) — should still be idempotent success.
	close3, _ := json.Marshal(map[string]any{"state": "closed"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/1", bytes.NewReader(close3))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("bare re-close: got %d want 200 (idempotent); body=%s", rr.Code, rr.Body.String())
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

// E4: issue list previously accepted (and silently dropped) `assignee`,
// `author`, `milestone`, `mention`. Each should now narrow correctly
// or 422 on unknown values.
func TestIssues_ListAssigneeFilter(t *testing.T) {
	ctx := context.Background()
	pool, router, aliceID, repoID, aliceToken := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")

	// Two issues; only #1 assigned to bob.
	for _, title := range []string{"assigned-to-bob", "unassigned"} {
		body, _ := json.Marshal(map[string]any{"title": title})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+aliceToken)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %q: %d", title, rr.Code)
		}
	}
	// Assign bob to issue #1 (lowest number).
	if err := issuesdb.New().AssignUserToIssue(ctx, pool, issuesdb.AssignUserToIssueParams{
		IssueID: issueIDByNumber(t, pool, repoID, 1), UserID: bobID,
		AssignedByUserID: pgtype.Int8{Int64: aliceID, Valid: true},
	}); err != nil {
		t.Fatalf("AssignUserToIssue: %v", err)
	}
	_ = aliceID

	cases := []struct {
		filter   string
		wantCode int
		wantLen  int
	}{
		{"assignee=bob", 200, 1},
		{"assignee=alice", 200, 0},
		{"assignee=ghost", 422, 0},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues?"+tc.filter, nil)
		req.Header.Set("Authorization", "Bearer "+aliceToken)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("%s: code=%d want %d; body=%s", tc.filter, rr.Code, tc.wantCode, rr.Body.String())
			continue
		}
		if tc.wantCode != 200 {
			continue
		}
		var rows []apiIssue
		_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		if len(rows) != tc.wantLen {
			t.Errorf("%s: got %d rows, want %d", tc.filter, len(rows), tc.wantLen)
		}
	}
}

func TestIssues_ListAuthorFilter(t *testing.T) {
	pool, router, _, _, aliceToken := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	// Two issues: alice + bob each create one. Alice owns the repo;
	// bob authored issue #2.
	for _, who := range []struct{ token, title string }{
		{aliceToken, "by-alice"}, {bobToken, "by-bob"},
	} {
		body, _ := json.Marshal(map[string]any{"title": who.title})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+who.token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %q: %d %s", who.title, rr.Code, rr.Body.String())
		}
	}

	for _, tc := range []struct {
		filter   string
		wantCode int
		wantLen  int
	}{
		{"author=alice", 200, 1},
		{"author=bob", 200, 1},
		{"author=ghost", 422, 0},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues?"+tc.filter, nil)
		req.Header.Set("Authorization", "Bearer "+aliceToken)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("%s: code=%d want %d; body=%s", tc.filter, rr.Code, tc.wantCode, rr.Body.String())
			continue
		}
		if tc.wantCode != 200 {
			continue
		}
		var rows []apiIssue
		_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		if len(rows) != tc.wantLen {
			t.Errorf("%s: got %d rows, want %d", tc.filter, len(rows), tc.wantLen)
		}
	}
}

func TestIssues_ListMilestoneFilter(t *testing.T) {
	ctx := context.Background()
	pool, router, _, repoID, token := seedIssuesEnv(t, "alice")

	m, err := issuesdb.New().CreateMilestone(ctx, pool, issuesdb.CreateMilestoneParams{
		RepoID: repoID, Title: "v1", DueOn: pgtype.Timestamptz{},
	})
	if err != nil {
		t.Fatalf("CreateMilestone: %v", err)
	}
	// Two issues; only #1 on the milestone.
	for _, payload := range []map[string]any{
		{"title": "on-milestone", "milestone": m.ID},
		{"title": "off-milestone"},
	} {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
		}
	}

	for _, tc := range []struct {
		filter   string
		wantCode int
		wantLen  int
	}{
		{fmt.Sprintf("milestone=%d", m.ID), 200, 1},
		// G5 (F13/F30): nonexistent ID now 422s instead of silent-empty
		// — list-vs-create consistency (create also 422s on bad ID).
		{"milestone=999", 422, 0},
		{"milestone=notanumber", 422, 0},
		{"milestone=0", 422, 0},
		{"milestone=-1", 422, 0},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues?"+tc.filter, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("%s: code=%d want %d; body=%s", tc.filter, rr.Code, tc.wantCode, rr.Body.String())
			continue
		}
		if tc.wantCode != 200 {
			continue
		}
		var rows []apiIssue
		_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		if len(rows) != tc.wantLen {
			t.Errorf("%s: got %d rows, want %d", tc.filter, len(rows), tc.wantLen)
		}
	}
}

func TestIssues_ListMentionRejectedExplicitly(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues?mention=alice", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("mention filter: code=%d want 422; body=%s", rr.Code, rr.Body.String())
	}
}

// G1: gh-canonical query aliases must land on the same validation path
// as the shithub-native spellings. Pre-fix the CLI sent `creator=`,
// `mentioned=`, and `label=` and the server silently dropped them —
// passing tests, broken filters. This pins the alias contract:
//   - author=  ↔ creator=
//   - mention= ↔ mentioned=
//   - labels=  ↔ label=
//
// Each pair must produce identical responses; an unknown user/label
// under either spelling must still 422.
func TestIssues_ListAcceptsGhCanonicalAliases(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	// Seed one issue so the author filter has a row to match.
	body, _ := json.Marshal(map[string]any{"title": "alice-issue"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed issue: %d %s", rr.Code, rr.Body.String())
	}

	get := func(query string) (int, []apiIssue) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		var rows []apiIssue
		if rr.Code == http.StatusOK {
			_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		}
		return rr.Code, rows
	}

	// author / creator — happy path matches; unknown 422s under either.
	if code, rows := get("author=alice"); code != 200 || len(rows) != 1 {
		t.Errorf("author=alice: code=%d rows=%d", code, len(rows))
	}
	if code, rows := get("creator=alice"); code != 200 || len(rows) != 1 {
		t.Errorf("creator=alice (alias): code=%d rows=%d", code, len(rows))
	}
	if code, _ := get("creator=ghost"); code != http.StatusUnprocessableEntity {
		t.Errorf("creator=ghost (alias unknown user): code=%d, want 422", code)
	}

	// mention / mentioned — both rejected with the same 422 (no support yet).
	if code, _ := get("mention=alice"); code != http.StatusUnprocessableEntity {
		t.Errorf("mention=alice: code=%d, want 422", code)
	}
	if code, _ := get("mentioned=alice"); code != http.StatusUnprocessableEntity {
		t.Errorf("mentioned=alice (alias): code=%d, want 422", code)
	}

	// labels / label — unknown label 422s under either spelling.
	if code, _ := get("labels=totally-fake"); code != http.StatusUnprocessableEntity {
		t.Errorf("labels=totally-fake: code=%d, want 422", code)
	}
	if code, _ := get("label=totally-fake"); code != http.StatusUnprocessableEntity {
		t.Errorf("label=totally-fake (alias): code=%d, want 422", code)
	}
}

func TestIssues_ListStateStrict(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	for _, tc := range []struct {
		state    string
		wantCode int
	}{
		{"open", 200},
		{"closed", 200},
		{"all", 200},
		{"", 200},
		{"nonsense", 422},
		{"merged", 422}, // PR-only; rejected on issues
		// H3 (H8): byte-exact match — silent normalization removed.
		{"OPEN", 422},
		{"open%20", 422},
		{"%20open", 422},
		{"open%0A", 422},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues?state="+tc.state, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("state=%q: code=%d want %d; body=%s", tc.state, rr.Code, tc.wantCode, rr.Body.String())
		}
	}
}

// issueIDByNumber resolves the issue.id for (repoID, number) — used by
// list-filter tests that need to attach assignees by id.
func issueIDByNumber(t *testing.T, pool *pgxpool.Pool, repoID, number int64) int64 {
	t.Helper()
	row, err := issuesdb.New().GetIssueByNumber(context.Background(), pool, issuesdb.GetIssueByNumberParams{
		RepoID: repoID, Number: number,
	})
	if err != nil {
		t.Fatalf("GetIssueByNumber: %v", err)
	}
	return row.ID
}

// E25: PATCH /issues/N used to succeed on archived repos because the
// resolver gated by ActionIssueRead (a read action that bypasses the
// archive write block). Now the issuePatch handler refuses with 403.
func TestIssues_PatchOnArchivedRepoIs403(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "before-archive"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}
	archiveRepoViaAPI(t, router, token, "alice", "demo")

	patch, _ := json.Marshal(map[string]any{"title": "after-archive"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/1", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("archived issue patch: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// G8 (F45): `DELETE /issues/{N}` admin-only, cascades via FKs, 204.
// Pre-G8 the CLI's `issue delete` got 405 against this verb and the
// command was vapor end-to-end. Pins:
//   - happy path returns 204 and the row is gone (GET 404 after)
//   - non-admin write collaborator gets 403 (gh's rule)
//   - PR numbers route to /issues/{N} return 404 (PRs delete via own
//     verb, future sprint)
func TestIssues_Delete(t *testing.T) {
	pool, router, _, _, token := seedIssuesEnv(t, "alice")

	// Create an issue + add a comment so the cascade has something
	// non-trivial to remove.
	body, _ := json.Marshal(map[string]any{"title": "to-delete", "body": "doomed"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed issue: %d", rr.Code)
	}
	cbody, _ := json.Marshal(map[string]any{"body": "child comment"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues/1/comments", bytes.NewReader(cbody))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed comment: %d", rr.Code)
	}

	// Non-admin write collaborator (bob) — 403.
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/issues/1", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin delete: code=%d want 403; body=%s", rr.Code, rr.Body.String())
	}

	// Admin (alice, repo owner) — 204 and the row is gone.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/issues/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("admin delete: code=%d want 204; body=%s", rr.Code, rr.Body.String())
	}
	// GET confirms cascade-deletion.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET after delete: code=%d want 404", rr.Code)
	}
}

// G8 (F45) boundary: deleting a PR number via /issues/{N} 404s.
// PRs share the issues table (kind='pr') but the bare GET surface is
// issue-only; the delete verb mirrors that contract.
func TestIssues_DeleteRejectsPRNumber(t *testing.T) {
	pool, router, _, _, token := seedIssuesEnv(t, "alice")
	// Use seedPullsEnv-style setup but the existing seedIssuesEnv
	// doesn't initialize a git dir — fake it by creating a kind='pr'
	// row directly via SQL since we just need the row to exist.
	// (The handler 404s before reading any other column.)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO issues (repo_id, number, kind, title, body, author_user_id, state)
		 VALUES ((SELECT id FROM repos WHERE name='demo'), 1, 'pr', 'fake pr', '', NULL, 'open')`)
	if err != nil {
		t.Fatalf("seed kind=pr row: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/issues/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("DELETE on PR number: code=%d want 404; body=%s", rr.Code, rr.Body.String())
	}
}
