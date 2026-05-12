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
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
)

type apiEvent struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Public  bool   `json:"public"`
	ActorID int64  `json:"actor_id"`
	RepoID  int64  `json:"repo_id"`
	Source  struct {
		Kind string `json:"kind"`
		ID   int64  `json:"id"`
	} `json:"source"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

// seedEvent inserts one domain_events row.
func seedEvent(t *testing.T, pool *pgxpool.Pool, actorID, repoID int64, kind string, public bool) int64 {
	t.Helper()
	row, err := socialdb.New().InsertDomainEvent(context.Background(), pool, socialdb.InsertDomainEventParams{
		ActorUserID: pgtype.Int8{Int64: actorID, Valid: actorID != 0},
		Kind:        kind,
		RepoID:      pgtype.Int8{Int64: repoID, Valid: repoID != 0},
		SourceKind:  "repo",
		SourceID:    repoID,
		Public:      public,
		Payload:     []byte(`{"hello":"world"}`),
	})
	if err != nil {
		t.Fatalf("InsertDomainEvent: %v", err)
	}
	return row.ID
}

func TestEvents_RepoFeedReturnsBothPublicAndPrivate(t *testing.T) {
	// On a repo-scoped feed, the caller has read access already so
	// the API returns both public and non-public events.
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	seedEvent(t, pool, userID, repoID, "pushed", true)
	seedEvent(t, pool, userID, repoID, "starred", false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiEvent
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) < 2 {
		t.Fatalf("expected ≥2 events; got %+v", listed)
	}
	// Recency-sorted: the most recent insert is first. starred came after pushed.
	if listed[0].Kind != "starred" {
		t.Errorf("recency-sort: first should be starred; got %+v", listed[0])
	}
	if listed[0].Source.Kind != "repo" || listed[0].Source.ID != repoID {
		t.Errorf("source shape: %+v", listed[0].Source)
	}
}

func TestEvents_UserFeedReturnsOnlyPublic(t *testing.T) {
	pool, router, userID, repoID, _ := seedIssuesEnv(t, "alice")
	// Use a bob token so the user feed isn't accidentally pulling
	// alice's own private rows via some implicit channel.
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeUserRead))
	seedEvent(t, pool, userID, repoID, "pushed", true)
	seedEvent(t, pool, userID, repoID, "private_thing", false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice/events", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiEvent
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	// repos.Create emits a `repo_created` public event already, so
	// the list also includes that. We assert: (a) `private_thing`
	// must NOT appear, (b) every returned row must be Public=true.
	var foundPushed, foundPrivate bool
	for _, ev := range listed {
		if !ev.Public {
			t.Errorf("non-public row leaked into user feed: %+v", ev)
		}
		if ev.Kind == "pushed" {
			foundPushed = true
		}
		if ev.Kind == "private_thing" {
			foundPrivate = true
		}
	}
	if !foundPushed {
		t.Errorf("expected public 'pushed' in user feed; got %+v", listed)
	}
	if foundPrivate {
		t.Errorf("non-public 'private_thing' leaked into user feed; got %+v", listed)
	}
}

func TestEvents_UserFeedUnknownUser404(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/ghost/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestEvents_PayloadRoundTrip(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	seedEvent(t, pool, userID, repoID, "issued", true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var listed []apiEvent
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) == 0 {
		t.Fatalf("no events returned")
	}
	var payload map[string]string
	if err := json.Unmarshal(listed[0].Payload, &payload); err != nil {
		t.Fatalf("payload decode: %v; raw=%s", err, string(listed[0].Payload))
	}
	if payload["hello"] != "world" {
		t.Errorf("payload round-trip: %+v", payload)
	}
}

func TestEvents_RepoFeedRequiresRepoRead(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	wrong := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/events", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestEvents_UserFeedRequiresUserRead(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	wrong := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice/events", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
