// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
)

type apiNotification struct {
	ID        int64  `json:"id"`
	Unread    bool   `json:"unread"`
	Reason    string `json:"reason"`
	Kind      string `json:"kind"`
	UpdatedAt string `json:"updated_at"`
	Subject   struct {
		Title  string `json:"title"`
		Type   string `json:"type"`
		Number int64  `json:"number"`
	} `json:"subject"`
}

// seedNotification inserts one threadless notification row for the
// supplied recipient. Returns the notification id.
func seedNotification(t *testing.T, pool *pgxpool.Pool, recipientID int64, reason string) int64 {
	t.Helper()
	row, err := notifdb.New().InsertThreadlessNotification(
		context.Background(), pool, notifdb.InsertThreadlessNotificationParams{
			RecipientUserID: recipientID,
			Kind:            "issue_assigned",
			Reason:          reason,
		},
	)
	if err != nil {
		t.Fatalf("InsertThreadlessNotification: %v", err)
	}
	return row.ID
}

func TestNotifications_ListReturnsUnread(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))
	seedNotification(t, pool, userID, "mention")
	seedNotification(t, pool, userID, "assigned")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiNotification
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("len: got %d, want 2; payload=%+v", len(listed), listed)
	}
	for _, n := range listed {
		if !n.Unread {
			t.Errorf("default list should be unread-only: %+v", n)
		}
	}
}

func TestNotifications_PatchMarksRead(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead), string(pat.ScopeUserWrite))
	notifID := seedNotification(t, pool, userID, "mention")

	// Default empty body → mark read.
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/notifications/threads/"+strconv.FormatInt(notifID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	// List with default (unread only) should now be empty.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, listReq)
	var listed []apiNotification
	_ = json.Unmarshal(listRR.Body.Bytes(), &listed)
	if len(listed) != 0 {
		t.Errorf("expected empty unread list after mark-read; got %+v", listed)
	}

	// List with ?all=true should include it as read.
	allReq := httptest.NewRequest(http.MethodGet, "/api/v1/notifications?all=true", nil)
	allReq.Header.Set("Authorization", "Bearer "+token)
	allRR := httptest.NewRecorder()
	router.ServeHTTP(allRR, allReq)
	var all []apiNotification
	_ = json.Unmarshal(allRR.Body.Bytes(), &all)
	if len(all) != 1 || all[0].Unread {
		t.Errorf("?all=true after mark-read: %+v", all)
	}
}

func TestNotifications_PatchMarksUnread(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead), string(pat.ScopeUserWrite))
	notifID := seedNotification(t, pool, userID, "mention")

	// Mark read first.
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/notifications/threads/"+strconv.FormatInt(notifID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	// Now flip back to unread.
	body, _ := json.Marshal(map[string]any{"unread": true})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/notifications/threads/"+strconv.FormatInt(notifID, 10), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("flip-unread status: got %d; body=%s", rr.Code, rr.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, listReq)
	var listed []apiNotification
	_ = json.Unmarshal(listRR.Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Errorf("expected 1 unread after flip; got %+v", listed)
	}
}

func TestNotifications_MarkAllRead(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead), string(pat.ScopeUserWrite))
	seedNotification(t, pool, userID, "mention")
	seedNotification(t, pool, userID, "assigned")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/notifications", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, listReq)
	var listed []apiNotification
	_ = json.Unmarshal(listRR.Body.Bytes(), &listed)
	if len(listed) != 0 {
		t.Errorf("expected empty unread after mark-all; got %+v", listed)
	}
}

func TestNotifications_CrossUserAccess404(t *testing.T) {
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeUserRead), string(pat.ScopeUserWrite))

	notifID := seedNotification(t, pool, aliceID, "mention")

	// Bob can't read or mutate alice's notification.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/threads/"+strconv.FormatInt(notifID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-user GET: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPatch, "/api/v1/notifications/threads/"+strconv.FormatInt(notifID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-user PATCH: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestNotifications_RequiresUserReadScope(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	wrong := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestNotifications_PatchRequiresUserWriteScope(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	readOnly := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))
	notifID := seedNotification(t, pool, userID, "mention")

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/notifications/threads/"+strconv.FormatInt(notifID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+readOnly)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
