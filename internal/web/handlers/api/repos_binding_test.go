// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

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

// PRO-EXT01-11b: end-to-end test that the API surface honors PAT repo
// binding. We seed two repos under one user, mint a token bound to
// repo A, and confirm:
//
//   - GET /api/v1/repos/alice/repo-A → 200 (binding matches)
//   - GET /api/v1/repos/alice/repo-B → 404 (binding mismatch surfaces
//     as "not found" so we don't leak that the binding caused the
//     deny — same shape as visibility-based 404s)

// seedAPIRepo inserts a minimal repo row directly via SQL — bypasses
// the repos.Create orchestrator (which needs RepoFS + Audit + Limiter
// wired). The /api/v1/repos/{owner}/{repo} GET path doesn't read from
// disk, so the bare row is enough to drive the test.
func seedAPIRepo(t *testing.T, pool *pgxpool.Pool, ownerUserID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO repos (owner_user_id, name, visibility, default_branch)
		VALUES ($1, $2, 'private', 'main') RETURNING id
	`, ownerUserID, name).Scan(&id); err != nil {
		t.Fatalf("seed api repo: %v", err)
	}
	return id
}

func TestAPIRepoBinding_BoundTokenSeesOnlyBoundRepo(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")

	repoAID := seedAPIRepo(t, pool, userID, "ci-target")
	_ = seedAPIRepo(t, pool, userID, "other-stuff")

	// Mint a PAT bound to repo A.
	rawTok, hash, prefix, err := pat.Mint()
	if err != nil {
		t.Fatalf("pat.Mint: %v", err)
	}
	if _, err := usersdb.New().InsertUserToken(context.Background(), pool, usersdb.InsertUserTokenParams{
		UserID:      userID,
		Name:        "bound test",
		TokenHash:   hash,
		TokenPrefix: prefix,
		Scopes:      []string{string(pat.ScopeRepoRead)},
		RepoID:      pgtype.Int8{Int64: repoAID, Valid: true},
	}); err != nil {
		t.Fatalf("InsertUserToken: %v", err)
	}

	// Bound repo: 200.
	reqA := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/ci-target", nil)
	reqA.Header.Set("Authorization", "Bearer "+rawTok)
	rrA := httptest.NewRecorder()
	router.ServeHTTP(rrA, reqA)
	if rrA.Code != http.StatusOK {
		t.Fatalf("bound repo GET: got %d, want 200; body=%s", rrA.Code, rrA.Body.String())
	}

	// Sibling repo: 404 (binding mismatch surfaces as "not found").
	reqB := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/other-stuff", nil)
	reqB.Header.Set("Authorization", "Bearer "+rawTok)
	rrB := httptest.NewRecorder()
	router.ServeHTTP(rrB, reqB)
	if rrB.Code != http.StatusNotFound {
		t.Fatalf("non-bound repo GET: got %d, want 404; body=%s", rrB.Code, rrB.Body.String())
	}
}

// TestAPIRepoBinding_UnboundTokenSeesAllOwnedRepos pins the inverse —
// an unbound token has no binding restriction.
func TestAPIRepoBinding_UnboundTokenSeesAllOwnedRepos(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	_ = seedAPIRepo(t, pool, userID, "anywhere")

	// Unbound token.
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/anywhere", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unbound GET: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}
