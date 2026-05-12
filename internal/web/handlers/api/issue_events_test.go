// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
)

type apiIssueEvent struct {
	ID            int64           `json:"id"`
	Kind          string          `json:"kind"`
	ActorUserID   int64           `json:"actor_user_id,omitempty"`
	ActorUsername string          `json:"actor_username,omitempty"`
	Meta          json.RawMessage `json:"meta,omitempty"`
	RefTargetID   int64           `json:"ref_target_id,omitempty"`
	CreatedAt     string          `json:"created_at"`
}

// seedIssueRow inserts an issue via the orchestrator and returns the
// row. Tests then layer events on top via seedIssueEvent.
func seedIssueRow(t *testing.T, pool *pgxpool.Pool, repoID, authorID int64, title string) issuesdb.Issue {
	t.Helper()
	row, err := issues.Create(context.Background(), issues.Deps{Pool: pool}, issues.CreateParams{
		RepoID:       repoID,
		AuthorUserID: authorID,
		Title:        title,
	})
	if err != nil {
		t.Fatalf("issues.Create: %v", err)
	}
	return row
}

// seedIssueEvent inserts one event row. Bypasses the orchestrators so
// each test can stage exactly the timeline it wants to assert against.
func seedIssueEvent(t *testing.T, pool *pgxpool.Pool, issueID, actorID int64, kind string, meta []byte) int64 {
	t.Helper()
	row, err := issuesdb.New().InsertIssueEvent(context.Background(), pool, issuesdb.InsertIssueEventParams{
		IssueID:     issueID,
		ActorUserID: pgtype.Int8{Int64: actorID, Valid: actorID != 0},
		Kind:        kind,
		Meta:        meta,
	})
	if err != nil {
		t.Fatalf("InsertIssueEvent(%q): %v", kind, err)
	}
	return row.ID
}

func TestIssueEvents_ListReturnsTimelineInChronologicalOrder(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	issue := seedIssueRow(t, pool, repoID, userID, "first")
	seedIssueEvent(t, pool, issue.ID, userID, "closed", []byte(`{"reason":"completed"}`))
	seedIssueEvent(t, pool, issue.ID, userID, "reopened", []byte(`{}`))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiIssueEvent
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("expected 2 events; got %+v", listed)
	}
	// ASC sort: closed first, reopened second.
	if listed[0].Kind != "closed" || listed[1].Kind != "reopened" {
		t.Errorf("chronological order: got %s, %s", listed[0].Kind, listed[1].Kind)
	}
	if listed[0].ActorUsername != "alice" {
		t.Errorf("actor_username: got %q, want alice", listed[0].ActorUsername)
	}
	if listed[0].ActorUserID != userID {
		t.Errorf("actor_user_id: got %d, want %d", listed[0].ActorUserID, userID)
	}
}

func TestIssueEvents_MetaIsRoundTripped(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	issue := seedIssueRow(t, pool, repoID, userID, "meta-test")
	seedIssueEvent(t, pool, issue.ID, userID, "labeled", []byte(`{"label_id":42,"label_name":"bug"}`))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiIssueEvent
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) == 0 {
		t.Fatalf("no events returned")
	}
	var meta map[string]any
	if err := json.Unmarshal(listed[0].Meta, &meta); err != nil {
		t.Fatalf("meta decode: %v; raw=%s", err, string(listed[0].Meta))
	}
	if meta["label_name"] != "bug" {
		t.Errorf("meta round-trip: got %+v", meta)
	}
}

func TestIssueEvents_RequiresRepoRead(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	wrong := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1/events", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssueEvents_UnknownIssue404(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/9999/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssueEvents_EmptyTimelineReturnsEmptyArray(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	seedIssueRow(t, pool, repoID, userID, "no-events-yet")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	// Empty slice, not null: a CLI shouldn't have to nil-check.
	if got := rr.Body.String(); got != "[]" && got != "[]\n" {
		t.Errorf("empty body: got %q, want []", got)
	}
}
