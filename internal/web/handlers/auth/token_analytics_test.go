// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// PRO-EXT01-11c: end-to-end handler tests for the per-token analytics
// page.
//
//   - Pro user → page renders with DB-backed counts.
//   - Free user → preview payload + IsPreview=true so the template
//     overlays the CTA.

// mintTokenForUser inserts a fresh PAT for userID and seeds a handful
// of usage events on different days so the aggregation queries have
// data to crunch.
func mintTokenForUser(t *testing.T, pool *pgxpool.Pool, userID int64) int64 {
	t.Helper()
	_, hash, prefix, err := pat.Mint()
	if err != nil {
		t.Fatalf("pat.Mint: %v", err)
	}
	row, err := usersdb.New().InsertUserToken(context.Background(), pool, usersdb.InsertUserTokenParams{
		UserID:      userID,
		Name:        "analytics-test",
		TokenHash:   hash,
		TokenPrefix: prefix,
		Scopes:      []string{string(pat.ScopeUserRead)},
	})
	if err != nil {
		t.Fatalf("InsertUserToken: %v", err)
	}
	seedUsageEvents(t, pool, row.ID)
	return row.ID
}

// seedUsageEvents drops a deterministic set of events for the token.
func seedUsageEvents(t *testing.T, pool *pgxpool.Pool, tokenID int64) {
	t.Helper()
	now := time.Now()
	events := []struct {
		offset       time.Duration
		method, path string
		status       int16
	}{
		{-time.Hour, "GET", "/api/v1/user", 200},
		{-2 * time.Hour, "GET", "/api/v1/user", 200},
		{-24 * time.Hour, "GET", "/api/v1/repos", 200},
		{-48 * time.Hour, "POST", "/api/v1/repos", 201},
	}
	for _, ev := range events {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO user_token_usage_events (token_id, occurred_at, method, route_prefix, status_code)
			VALUES ($1, $2, $3, $4, $5)
		`, tokenID, now.Add(ev.offset), ev.method, ev.path, ev.status); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}
}

func TestTokenAnalytics_ProUserSeesRealCounts(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{})
	cli, userID := signupAndLoginFor(t, srv, pool, captor, "alice11canalytics")
	upgradeProfileTestUserToPro(t, pool, userID)
	tokenID := mintTokenForUser(t, pool, userID)

	resp := cli.get(t, "/settings/tokens/"+strconv.FormatInt(tokenID, 10)+"/analytics")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("analytics GET: %d body=%s", resp.StatusCode, body)
	}
	str := string(body)
	if !strings.Contains(str, "PREVIEW=false") {
		t.Errorf("Pro user should not see preview: %s", str)
	}
	if !strings.Contains(str, "TOTAL=4;") {
		t.Errorf("expected TOTAL=4 from seeded events; got %s", str)
	}
	if !strings.Contains(str, "/api/v1/user") || !strings.Contains(str, "/api/v1/repos") {
		t.Errorf("top routes should include seeded prefixes: %s", str)
	}
}

func TestTokenAnalytics_FreeUserSeesPreview(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{})
	cli, userID := signupAndLoginFor(t, srv, pool, captor, "alice11cfree")
	tokenID := mintTokenForUser(t, pool, userID)

	resp := cli.get(t, "/settings/tokens/"+strconv.FormatInt(tokenID, 10)+"/analytics")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("analytics GET: %d body=%s", resp.StatusCode, body)
	}
	str := string(body)
	if !strings.Contains(str, "PREVIEW=true") {
		t.Errorf("Free user should see preview: %s", str)
	}
	// Preview shows synthetic top routes — confirm at least one is there.
	if !strings.Contains(str, "/api/v1/repos") {
		t.Errorf("preview should include synthetic top routes: %s", str)
	}
	// Preview total is 0 (we never count real events for Free).
	if !strings.Contains(str, "TOTAL=0;") {
		t.Errorf("preview total should be 0: %s", str)
	}
}

func TestTokenAnalytics_RejectsOtherUsersToken(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{})

	// Bob signs up first and mints a token.
	_, bobID := signupAndLoginFor(t, srv, pool, captor, "bob11canalytics")
	bobTokenID := mintTokenForUser(t, pool, bobID)

	// Alice signs in (fresh client) and tries to view Bob's analytics.
	captor.reset()
	aliceCli, _ := signupAndLoginFor(t, srv, pool, captor, "alice11cattacker")
	resp := aliceCli.get(t, "/settings/tokens/"+strconv.FormatInt(bobTokenID, 10)+"/analytics")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user analytics access: got %d, want 404", resp.StatusCode)
	}
}
