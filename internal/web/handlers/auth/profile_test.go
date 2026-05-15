// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
)

func loginProfileUser(t *testing.T) *client {
	t.Helper()
	httpsrv, captor := newTestServer(t, false)
	cli := newClient(t, httpsrv)

	mustSignup(t, cli, "alicep", "alicep@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"alicep"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	return cli
}

func TestProfileEditor_Roundtrip(t *testing.T) {
	t.Parallel()
	cli := loginProfileUser(t)

	// GET shows the form.
	resp := cli.get(t, "/settings/profile")
	if resp.StatusCode != 200 {
		t.Fatalf("get: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Public profile") {
		t.Fatalf("missing heading: %s", body)
	}

	csrf := cli.extractCSRF(t, "/settings/profile")
	resp = cli.post(t, "/settings/profile", url.Values{
		"csrf_token":   {csrf},
		"display_name": {"Alice P."},
		"bio":          {"Building things."},
		"location":     {"Berlin"},
		"website":      {"example.com"}, // bare host -> auto https://
		"company":      {"Acme"},
		"pronouns":     {"she/her"},
	})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("post: %d %s", resp.StatusCode, body)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{"Profile updated.", "Alice P.", "Building things.", "Berlin", "https://example.com", "Acme", "she/her"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q in body", want)
		}
	}
}

func TestProfileEditor_RejectsBadURL(t *testing.T) {
	t.Parallel()
	cli := loginProfileUser(t)
	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := cli.post(t, "/settings/profile", url.Values{
		"csrf_token": {csrf},
		"website":    {"javascript:alert(1)"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "http(s)") {
		t.Fatalf("expected URL error, got: %s", body)
	}
}

func TestProfileEditor_RequiresAuth(t *testing.T) {
	t.Parallel()
	httpsrv, _ := newTestServer(t, false)
	cli := newClient(t, httpsrv)
	resp := cli.get(t, "/settings/profile")
	defer func() { _ = resp.Body.Close() }()
	// RequireUser sends an unauthenticated visitor to /login (303 See Other).
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect, got %d", resp.StatusCode)
	}
}

func TestProfileEditor_TooLongBio(t *testing.T) {
	t.Parallel()
	cli := loginProfileUser(t)
	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := cli.post(t, "/settings/profile", url.Values{
		"csrf_token": {csrf},
		"bio":        {strings.Repeat("a", 501)},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Bio is too long") {
		t.Fatalf("expected bio length error, got: %s", body)
	}
}

// PRO-EXT01-04: vanity write is gated on FeatureProfileVanity. Free
// user submits accent/layout values → handler drops them silently and
// the DB row keeps its defaults. Pro user submits the same values →
// they persist.
func TestProfileEditor_VanityValuesGatedOnPro(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "vanityalice")

	// Free attempt — values must NOT persist.
	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := cli.post(t, "/settings/profile", url.Values{
		"csrf_token":     {csrf},
		"display_name":   {"Alice"},
		"accent_hex":     {"#ff00ff"},
		"profile_layout": {"featured"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Free POST: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	assertVanityValues(t, pool, userID, "", "list")

	// Upgrade to Pro and retry — values should persist.
	upgradeProfileTestUserToPro(t, pool, userID)
	csrf = cli.extractCSRF(t, "/settings/profile")
	resp = cli.post(t, "/settings/profile", url.Values{
		"csrf_token":     {csrf},
		"display_name":   {"Alice"},
		"accent_hex":     {"#ff00ff"},
		"profile_layout": {"featured"},
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Pro POST: %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()
	assertVanityValues(t, pool, userID, "#ff00ff", "featured")
}

// TestProfileEditor_VanityRejectsMalformedAccent — a Pro user posting
// a bogus accent hex must fail validation with a friendly message
// (defense-in-depth against the DB CHECK constraint).
func TestProfileEditor_VanityRejectsMalformedAccent(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "badcoloralice")
	upgradeProfileTestUserToPro(t, pool, userID)

	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := cli.post(t, "/settings/profile", url.Values{
		"csrf_token":     {csrf},
		"accent_hex":     {"red"}, // not a #rrggbb
		"profile_layout": {"list"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Accent color must be") {
		t.Fatalf("expected accent error, got: %s", body)
	}
}

func assertVanityValues(t *testing.T, pool *pgxpool.Pool, userID int64, wantAccent, wantLayout string) {
	t.Helper()
	var gotAccent, gotLayout string
	if err := pool.QueryRow(context.Background(),
		`SELECT profile_accent_hex, profile_layout FROM users WHERE id = $1`, userID,
	).Scan(&gotAccent, &gotLayout); err != nil {
		t.Fatalf("read vanity values: %v", err)
	}
	if gotAccent != wantAccent {
		t.Errorf("accent: got %q, want %q", gotAccent, wantAccent)
	}
	if gotLayout != wantLayout {
		t.Errorf("layout: got %q, want %q", gotLayout, wantLayout)
	}
}

func upgradeProfileTestUserToPro(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	now := time.Now().UTC()
	suffix := strconv.FormatInt(userID, 10)
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: billingPgText("sub_vanity_pro_test_" + suffix),
		CurrentPeriodStart:   billingPgTime(now.Add(-time.Hour)),
		CurrentPeriodEnd:     billingPgTime(now.Add(30 * 24 * time.Hour)),
		LastWebhookEventID:   "evt_vanity_pro_test_" + suffix,
	})
	if err != nil {
		t.Fatalf("ApplyUserSubscriptionSnapshot: %v", err)
	}
}

// Suppress unused-import nag when httptest type isn't referenced
// (the import is required for *httptest.Server in newBillingTestUser
// signatures consumed via helper).
var _ = httptest.NewServer
