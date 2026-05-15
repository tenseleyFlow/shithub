// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// PRO-EXT01-11a: middleware enforcement contract for the IP allowlist.

// These tests exercise the security-critical read path: once a token has
// a non-empty allowlist, ANY request from outside it must be denied —
// independent of the enforce flag, which gates only the write path.

// seedTokenWithAllowlist directly inserts a user + token via sqlc.
// Returns the raw PAT (caller embeds in Authorization) and the pool
// (so the middleware shares the connection). Bypasses signup/login to
// keep the test focused on the middleware contract.
func seedTokenWithAllowlist(t *testing.T, allowlist []string) (raw string, pool *pgxpool.Pool) {
	t.Helper()
	pool = dbtest.NewTestDB(t)
	q := usersdb.New()
	ctx := context.Background()

	slug := tNameSlug(t)
	var userID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, session_epoch)
		VALUES ($1, '!', 1) RETURNING id
	`, "ipallow"+slug).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	rawTok, tokenHash, prefix, err := pat.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := q.InsertUserToken(ctx, pool, usersdb.InsertUserTokenParams{
		UserID:      userID,
		Name:        "test",
		TokenHash:   tokenHash,
		TokenPrefix: prefix,
		Scopes:      []string{"user:read"},
		IpAllowlist: allowlist, // nil → COALESCE → '{}' (no restriction)
	}); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	return rawTok, pool
}

// tNameSlug returns a unique-per-test slug for username uniqueness.
// The users.username column caps at 39 characters; we hash to a fixed
// 12-char hex so even deeply-nested subtest names fit comfortably under
// the "ipallow"+slug prefix budget.
func tNameSlug(t *testing.T) string {
	t.Helper()
	h := sha256.Sum256([]byte(t.Name()))
	return hex.EncodeToString(h[:6])
}

// dispatchPATRequest builds a recorder, sets req.RemoteAddr (which the
// middleware reads via remoteAddrFromRequest's RemoteAddr fallback),
// and runs the middleware against an inner OK handler.
func dispatchPATRequest(pool *pgxpool.Pool, raw, remoteAddr string) *httptest.ResponseRecorder {
	mw := PATAuthMiddleware(PATConfig{
		Pool:      pool,
		Debouncer: pat.NewDebouncer(0),
		Logger:    dropLogger(),
	})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "token "+raw)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	mw(inner).ServeHTTP(rec, req)
	return rec
}

// TestPATMiddleware_AllowlistAllowsContainedIP confirms the positive
// case: a request from inside the allowlist passes through.
func TestPATMiddleware_AllowlistAllowsContainedIP(t *testing.T) {
	t.Parallel()
	raw, pool := seedTokenWithAllowlist(t, []string{"203.0.113.0/24"})
	rec := dispatchPATRequest(pool, raw, "203.0.113.5:34567")
	if rec.Code != http.StatusOK {
		t.Fatalf("contained IP should pass: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPATMiddleware_AllowlistRejectsOutsideIP is the security-critical
// case: a request from outside the allowlist must be rejected with
// 401 + WWW-Authenticate explaining the denial reason.
func TestPATMiddleware_AllowlistRejectsOutsideIP(t *testing.T) {
	t.Parallel()
	raw, pool := seedTokenWithAllowlist(t, []string{"203.0.113.0/24"})
	rec := dispatchPATRequest(pool, raw, "198.51.100.5:34567")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("outside IP should be 401: status=%d body=%s", rec.Code, rec.Body.String())
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(wa, "not authorized from this address") {
		t.Fatalf("WWW-Authenticate missing reason: %q", wa)
	}
}

// TestPATMiddleware_EmptyAllowlistMatchesAny pins the "no restriction"
// invariant — Free users (and Pro users who didn't attach an allowlist)
// see no IP gate at all.
func TestPATMiddleware_EmptyAllowlistMatchesAny(t *testing.T) {
	t.Parallel()
	raw, pool := seedTokenWithAllowlist(t, nil)
	rec := dispatchPATRequest(pool, raw, "198.51.100.5:34567")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty allowlist should match any IP: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPATMiddleware_AllowlistRejectsWhenRemoteAddrUnparseable confirms
// fail-closed behavior when the request lacks a parseable client IP.
// In production this can't happen (chi populates RemoteAddr from the
// raw conn), but the middleware must defend against it anyway.
func TestPATMiddleware_AllowlistRejectsWhenRemoteAddrUnparseable(t *testing.T) {
	t.Parallel()
	raw, pool := seedTokenWithAllowlist(t, []string{"203.0.113.0/24"})
	rec := dispatchPATRequest(pool, raw, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing remote addr should fail closed: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPATMiddleware_AllowlistAcceptsBareIPv4Address confirms the
// canonical-storage contract: a single bare address stored as /32
// matches exactly that address.
func TestPATMiddleware_AllowlistAcceptsBareIPv4Address(t *testing.T) {
	t.Parallel()
	raw, pool := seedTokenWithAllowlist(t, []string{"203.0.113.5/32"})
	if rec := dispatchPATRequest(pool, raw, "203.0.113.5:34567"); rec.Code != http.StatusOK {
		t.Fatalf("exact-match /32 should pass: status=%d", rec.Code)
	}
	if rec := dispatchPATRequest(pool, raw, "203.0.113.6:34567"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-matching /32 should be 401: status=%d", rec.Code)
	}
}
