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
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

type apiUserEmail struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func seedUserEmails(t *testing.T, pool *pgxpool.Pool, userID int64) (verifiedID, unverifiedID int64) {
	t.Helper()
	ctx := context.Background()
	q := usersdb.New()

	verified, err := q.CreateUserEmail(ctx, pool, usersdb.CreateUserEmailParams{
		UserID:    userID,
		Email:     "alice+primary@example.test",
		IsPrimary: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail primary: %v", err)
	}
	if err := q.MarkUserEmailVerified(ctx, pool, verified.ID); err != nil {
		t.Fatalf("MarkUserEmailVerified: %v", err)
	}
	unverified, err := q.CreateUserEmail(ctx, pool, usersdb.CreateUserEmailParams{
		UserID:                userID,
		Email:                 "alice+work@example.test",
		IsPrimary:             false,
		Verified:              false,
		VerificationTokenHash: []byte("deadbeef"),
	})
	if err != nil {
		t.Fatalf("CreateUserEmail secondary: %v", err)
	}
	return verified.ID, unverified.ID
}

func TestUserEmails_ListAll(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	verifiedID, unverifiedID := seedUserEmails(t, pool, userID)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/emails", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var emails []apiUserEmail
	if err := json.Unmarshal(rr.Body.Bytes(), &emails); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if len(emails) != 2 {
		t.Fatalf("count: got %d, want 2; body=%s", len(emails), rr.Body.String())
	}
	ids := map[int64]apiUserEmail{}
	for _, em := range emails {
		ids[em.ID] = em
	}
	if !ids[verifiedID].Verified || !ids[verifiedID].Primary {
		t.Errorf("primary verified email mis-rendered: %+v", ids[verifiedID])
	}
	if ids[unverifiedID].Verified || ids[unverifiedID].Primary {
		t.Errorf("secondary unverified email mis-rendered: %+v", ids[unverifiedID])
	}
}

func TestUserEmails_FilterVerifiedTrue(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	seedUserEmails(t, pool, userID)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/emails?verified=true", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var emails []apiUserEmail
	if err := json.Unmarshal(rr.Body.Bytes(), &emails); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(emails) != 1 {
		t.Fatalf("verified=true count: got %d, want 1", len(emails))
	}
	if !emails[0].Verified {
		t.Errorf("verified=true returned an unverified row: %+v", emails[0])
	}
}

func TestUserEmails_FilterVerifiedFalse(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	seedUserEmails(t, pool, userID)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/emails?verified=false", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var emails []apiUserEmail
	if err := json.Unmarshal(rr.Body.Bytes(), &emails); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(emails) != 1 {
		t.Fatalf("verified=false count: got %d, want 1", len(emails))
	}
	if emails[0].Verified {
		t.Errorf("verified=false returned a verified row: %+v", emails[0])
	}
}

func TestUserEmails_RequiresScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	// Token has admin:read only — neither user:read nor user:write.
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeAdminRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/emails", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserEmails_Unauthenticated(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/emails", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}
