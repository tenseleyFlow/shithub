// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// PRO-EXT01-11b: the middleware does NOT 4xx on binding mismatch — the
// route handler that resolves a repo is the right enforcement point.
// What the middleware MUST do is populate PATAuth.RepoBinding so the
// handlers downstream can compare. These tests pin that propagation
// contract.

// seedTokenWithRepoBinding inserts a user, a repo owned by that user,
// and a PAT bound to the repo (or unbound, if withBinding=false).
func seedTokenWithRepoBinding(t *testing.T, withBinding bool) (raw string, pool *pgxpool.Pool, repoID int64) {
	t.Helper()
	pool = dbtest.NewTestDB(t)
	q := usersdb.New()
	ctx := context.Background()

	slug := tNameSlug(t)
	var userID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, session_epoch)
		VALUES ($1, '!', 1) RETURNING id
	`, "ptbind"+slug).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO repos (owner_user_id, name, visibility, default_branch)
		VALUES ($1, 'bound-repo', 'private', 'main') RETURNING id
	`, userID).Scan(&repoID); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	rawTok, tokenHash, prefix, err := pat.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	binding := pgtype.Int8{}
	if withBinding {
		binding = pgtype.Int8{Int64: repoID, Valid: true}
	}
	if _, err := q.InsertUserToken(ctx, pool, usersdb.InsertUserTokenParams{
		UserID:      userID,
		Name:        "test",
		TokenHash:   tokenHash,
		TokenPrefix: prefix,
		Scopes:      []string{"user:read"},
		IpAllowlist: nil,
		RepoID:      binding,
	}); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	return rawTok, pool, repoID
}

// captureAuth runs PATAuthMiddleware in front of a snapshot handler and
// returns the resolved PATAuth.
func captureAuth(t *testing.T, pool *pgxpool.Pool, raw string) PATAuth {
	t.Helper()
	var got PATAuth
	mw := PATAuthMiddleware(PATConfig{
		Pool:      pool,
		Debouncer: pat.NewDebouncer(0),
		Logger:    dropLogger(),
	})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = PATAuthFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "token "+raw)
	req.RemoteAddr = "203.0.113.5:34567"
	rec := httptest.NewRecorder()
	mw(inner).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("middleware short-circuited: code=%d body=%s", rec.Code, rec.Body.String())
	}
	return got
}

// TestPATMiddleware_BoundTokenPopulatesRepoBinding confirms that a
// bound token surfaces the binding via PATAuth.RepoBinding so route
// handlers can compare against the request's resolved repo.
func TestPATMiddleware_BoundTokenPopulatesRepoBinding(t *testing.T) {
	t.Parallel()
	raw, pool, repoID := seedTokenWithRepoBinding(t, true)
	got := captureAuth(t, pool, raw)
	if got.RepoBinding != repoID {
		t.Fatalf("RepoBinding: got %d, want %d", got.RepoBinding, repoID)
	}
}

// TestPATMiddleware_UnboundTokenReportsZeroBinding pins the inverse:
// an unbound token surfaces RepoBinding=0 (= "no restriction"), the
// pat.RepoBindingAllows helper treats this as match-anything.
func TestPATMiddleware_UnboundTokenReportsZeroBinding(t *testing.T) {
	t.Parallel()
	raw, pool, _ := seedTokenWithRepoBinding(t, false)
	got := captureAuth(t, pool, raw)
	if got.RepoBinding != 0 {
		t.Fatalf("unbound token should report RepoBinding=0, got %d", got.RepoBinding)
	}
}
