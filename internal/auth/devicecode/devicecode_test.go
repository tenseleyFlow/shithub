// SPDX-License-Identifier: AGPL-3.0-or-later

package devicecode_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/devicecode"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// fixedArgon2Hash is a pre-computed argon2id hash of an arbitrary password.
// Using a constant avoids running argon2 in every test setup.
const fixedArgon2Hash = "$argon2id$v=19$m=1024,t=1,p=1$YWFhYWFhYWFhYWFhYWFhYQ$" +
	"DvBOTSnFhCBe+Pfx/W7Sk3hG3JCm2Wj0RBgCu+CPDtY"

func seedUser(t *testing.T, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	q := usersdb.New()
	u, err := q.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username:     username,
		DisplayName:  strings.ToUpper(username[:1]) + username[1:],
		PasswordHash: fixedArgon2Hash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	em, err := q.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID:    u.ID,
		Email:     username + "@example.test",
		IsPrimary: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	if err := q.MarkUserEmailVerified(context.Background(), pool, em.ID); err != nil {
		t.Fatalf("MarkUserEmailVerified: %v", err)
	}
	return u.ID
}

func defaultsForTest() devicecode.Config {
	return devicecode.Config{
		ClientIDs:     []string{"shithub-cli"},
		DefaultScopes: []string{"user:read"},
		ExpiresIn:     15 * time.Minute,
		PollInterval:  5 * time.Second,
	}
}

func TestCreate_HappyPath(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	deps := devicecode.Deps{Pool: pool}
	auth, err := devicecode.Create(context.Background(), deps, defaultsForTest(), "shithub-cli", "user:read,repo:read")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if auth.DeviceCode == "" || auth.UserCode == "" {
		t.Fatalf("Create returned empty codes: %+v", auth)
	}
	if !strings.Contains(auth.UserCode, "-") || len(auth.UserCode) != 9 {
		t.Errorf("user_code shape: got %q", auth.UserCode)
	}
	if auth.ExpiresIn != 15*time.Minute {
		t.Errorf("expires_in: got %s", auth.ExpiresIn)
	}
	if got, want := auth.Scopes, []string{"user:read", "repo:read"}; !equalStrings(got, want) {
		t.Errorf("scopes: got %v, want %v", got, want)
	}
}

func TestCreate_RejectsUnknownClient(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	_, err := devicecode.Create(context.Background(), devicecode.Deps{Pool: pool}, defaultsForTest(), "evil-cli", "")
	if !errors.Is(err, devicecode.ErrUnauthorizedClient) {
		t.Fatalf("got err %v, want ErrUnauthorizedClient", err)
	}
}

func TestCreate_RejectsUnknownScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	_, err := devicecode.Create(context.Background(), devicecode.Deps{Pool: pool}, defaultsForTest(), "shithub-cli", "user:read,bogus:scope")
	if !errors.Is(err, devicecode.ErrInvalidScope) {
		t.Fatalf("got err %v, want ErrInvalidScope", err)
	}
}

func TestExchange_PendingThenApprovedThenOneShot(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	deps := devicecode.Deps{Pool: pool}
	userID := seedUser(t, pool, "alice")

	auth, err := devicecode.Create(context.Background(), deps, defaultsForTest(), "shithub-cli", "user:read")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Pending: Exchange returns authorization_pending.
	if _, err := devicecode.Exchange(context.Background(), deps, "shithub-cli", auth.DeviceCode, "test"); !errors.Is(err, devicecode.ErrAuthorizationPending) {
		t.Fatalf("pending: got %v, want ErrAuthorizationPending", err)
	}

	row, err := devicecode.LookupByUserCode(context.Background(), deps, auth.UserCode)
	if err != nil {
		t.Fatalf("LookupByUserCode: %v", err)
	}
	if err := devicecode.Approve(context.Background(), deps, row.ID, userID); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Advance last_polled_at so the slow_down gate doesn't fire on the
	// second Exchange below; otherwise we'd race the 5-second window.
	if _, err := pool.Exec(context.Background(),
		"UPDATE device_authorizations SET last_polled_at = now() - interval '10 seconds' WHERE id = $1",
		row.ID); err != nil {
		t.Fatalf("rewind last_polled_at: %v", err)
	}

	res, err := devicecode.Exchange(context.Background(), deps, "shithub-cli", auth.DeviceCode, "test")
	if err != nil {
		t.Fatalf("Exchange after approve: %v", err)
	}
	if !strings.HasPrefix(res.AccessToken, "shithub_pat_") {
		t.Errorf("access_token prefix: got %q", res.AccessToken)
	}
	if res.TokenType != "bearer" {
		t.Errorf("token_type: got %q", res.TokenType)
	}
	if got, want := res.Scopes, []string{"user:read"}; !equalStrings(got, want) {
		t.Errorf("scopes: got %v, want %v", got, want)
	}

	// One-shot lockout: a second Exchange must NOT re-issue.
	if _, err := pool.Exec(context.Background(),
		"UPDATE device_authorizations SET last_polled_at = now() - interval '10 seconds' WHERE id = $1",
		row.ID); err != nil {
		t.Fatalf("rewind last_polled_at: %v", err)
	}
	if _, err := devicecode.Exchange(context.Background(), deps, "shithub-cli", auth.DeviceCode, "test"); !errors.Is(err, devicecode.ErrInvalidGrant) {
		t.Fatalf("one-shot: got %v, want ErrInvalidGrant", err)
	}
}

func TestExchange_DeniedReturnsAccessDenied(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	deps := devicecode.Deps{Pool: pool}
	auth, err := devicecode.Create(context.Background(), deps, defaultsForTest(), "shithub-cli", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row, _ := devicecode.LookupByUserCode(context.Background(), deps, auth.UserCode)
	if err := devicecode.Deny(context.Background(), deps, row.ID); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if _, err := devicecode.Exchange(context.Background(), deps, "shithub-cli", auth.DeviceCode, "test"); !errors.Is(err, devicecode.ErrAccessDenied) {
		t.Fatalf("denied: got %v, want ErrAccessDenied", err)
	}
}

func TestExchange_ExpiredReturnsExpiredToken(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	deps := devicecode.Deps{Pool: pool}
	auth, err := devicecode.Create(context.Background(), deps, defaultsForTest(), "shithub-cli", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Backdate expires_at past now().
	if _, err := pool.Exec(context.Background(),
		"UPDATE device_authorizations SET expires_at = now() - interval '1 minute' WHERE user_code = $1",
		auth.UserCode); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}
	if _, err := devicecode.Exchange(context.Background(), deps, "shithub-cli", auth.DeviceCode, "test"); !errors.Is(err, devicecode.ErrExpiredToken) {
		t.Fatalf("expired: got %v, want ErrExpiredToken", err)
	}
}

func TestExchange_SlowDownAfterFastPoll(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	deps := devicecode.Deps{Pool: pool}
	auth, err := devicecode.Create(context.Background(), deps, defaultsForTest(), "shithub-cli", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// First poll lands; expect authorization_pending and a stamped last_polled_at.
	if _, err := devicecode.Exchange(context.Background(), deps, "shithub-cli", auth.DeviceCode, "test"); !errors.Is(err, devicecode.ErrAuthorizationPending) {
		t.Fatalf("first poll: got %v, want ErrAuthorizationPending", err)
	}
	// Immediate re-poll inside the interval → slow_down.
	if _, err := devicecode.Exchange(context.Background(), deps, "shithub-cli", auth.DeviceCode, "test"); !errors.Is(err, devicecode.ErrSlowDown) {
		t.Fatalf("second poll: got %v, want ErrSlowDown", err)
	}
}

func TestExchange_WrongClientIDReturnsUnauthorizedClient(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	deps := devicecode.Deps{Pool: pool}
	cfg := defaultsForTest()
	cfg.ClientIDs = []string{"shithub-cli", "other-cli"}
	auth, err := devicecode.Create(context.Background(), deps, cfg, "shithub-cli", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := devicecode.Exchange(context.Background(), deps, "other-cli", auth.DeviceCode, "test"); !errors.Is(err, devicecode.ErrUnauthorizedClient) {
		t.Fatalf("got %v, want ErrUnauthorizedClient", err)
	}
}

func TestApprove_RejectsAlreadyTerminal(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	deps := devicecode.Deps{Pool: pool}
	userID := seedUser(t, pool, "alice")
	auth, _ := devicecode.Create(context.Background(), deps, defaultsForTest(), "shithub-cli", "")
	row, _ := devicecode.LookupByUserCode(context.Background(), deps, auth.UserCode)
	if err := devicecode.Approve(context.Background(), deps, row.ID, userID); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	// Manually mark approved_at via Approve being called against an
	// already-approved row.
	if err := devicecode.Approve(context.Background(), deps, row.ID, userID); !errors.Is(err, devicecode.ErrAlreadyTerminal) {
		t.Fatalf("second approve: got %v, want ErrAlreadyTerminal", err)
	}
}

// equalStrings is a tiny slice-equality helper to keep imports light.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Sanity: the orchestrator must persist a row that the lookup query
// can find.
func TestCreate_PersistsRow(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	deps := devicecode.Deps{Pool: pool}
	auth, err := devicecode.Create(context.Background(), deps, defaultsForTest(), "shithub-cli", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row, err := usersdb.New().GetDeviceAuthorizationByUserCode(context.Background(), pool, auth.UserCode)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if row.ApprovedAt.Valid || row.DeniedAt.Valid {
		t.Errorf("fresh row should be pending; got approved=%v denied=%v",
			row.ApprovedAt, row.DeniedAt)
	}
	if !row.ExpiresAt.Valid || row.ExpiresAt.Time.Before(time.Now()) {
		t.Errorf("expires_at not in the future: %+v", row.ExpiresAt)
	}
	if row.UserID.Valid {
		t.Errorf("user_id should be unset before approval; got %v", row.UserID)
	}
}

// Defensive: a pgx.Int8 zero-value should marshal cleanly when we
// Approve a row that uses it for IssuedTokenID. This is a sanity check
// that the Approve query accepts a "null" issued_token_id at SQL level
// (we set it later in Exchange).
func TestApprove_SetsUserIDOnly(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	deps := devicecode.Deps{Pool: pool}
	userID := seedUser(t, pool, "alice")
	auth, _ := devicecode.Create(context.Background(), deps, defaultsForTest(), "shithub-cli", "")
	row, _ := devicecode.LookupByUserCode(context.Background(), deps, auth.UserCode)
	if err := devicecode.Approve(context.Background(), deps, row.ID, userID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	got, err := usersdb.New().GetDeviceAuthorizationByUserCode(context.Background(), pool, auth.UserCode)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !got.ApprovedAt.Valid {
		t.Errorf("approved_at not set")
	}
	if got.UserID != (pgtype.Int8{Int64: userID, Valid: true}) {
		t.Errorf("user_id: got %+v, want %d", got.UserID, userID)
	}
	if got.IssuedTokenID.Valid {
		t.Errorf("issued_token_id should be set only at Exchange time; got %+v", got.IssuedTokenID)
	}
}
