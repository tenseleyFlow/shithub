// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/password"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	authh "github.com/tenseleyFlow/shithub/internal/web/handlers/auth"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// PRO-EXT01-11a: end-to-end tests for the IP-allowlist write path on
// /settings/tokens. The middleware enforcement path is exercised in
// internal/web/middleware/pat_test.go; here we pin the handler contract:
//
//   - Free user, enforce off → allowlist accepted, recorded report-only.
//   - Free user, enforce on  → allowlist rejected with upgrade banner.
//   - Pro user, enforce on   → allowlist accepted.
//   - Malformed CIDR always rejected with a friendly error.

// newTokenServerWithEnforce mirrors newTokenServer but exposes the pool
// so tests can flip a user to Pro, and accepts a BillingEnforce config
// so we can exercise both report-only and enforce paths.
func newTokenServerWithEnforce(t *testing.T, enforce config.EnforceConfig) (*httptest.Server, *pgxpool.Pool, *captureSender) {
	t.Helper()
	pool := dbtest.NewTestDB(t)

	rr, err := render.New(authTemplatesFS(), render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	storeKey, _ := session.GenerateKey()
	store, _ := session.NewCookieStore(session.CookieStoreConfig{
		Key: storeKey, MaxAge: time.Hour, Secure: false,
	})

	captor := &captureSender{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	totpKey, _ := secretbox.GenerateKey()
	box, _ := secretbox.FromBytes(totpKey)

	authH, err := authh.New(authh.Deps{
		Logger: logger, Render: rr, Pool: pool, SessionStore: store, Email: captor,
		Branding:                 email.Branding{SiteName: "shithub", BaseURL: "http://test.invalid", From: "noreply@x"},
		Argon2:                   password.Params{Memory: 16 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
		Limiter:                  throttle.NewLimiter(),
		RequireEmailVerification: false,
		SecretBox:                box,
		BillingEnforce:           enforce,
	})
	if err != nil {
		t.Fatalf("authh.New: %v", err)
	}
	apiH, err := apih.New(apih.Deps{Pool: pool, Debouncer: pat.NewDebouncer(60 * time.Second)})
	if err != nil {
		t.Fatalf("apih.New: %v", err)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP(middleware.RealIPConfig{}))
	r.Use(middleware.SessionLoader(store, logger))
	r.Use(middleware.OptionalUser(func(ctx context.Context, id int64) (middleware.UserLookupResult, error) {
		c, err := pool.Acquire(ctx)
		if err != nil {
			return middleware.UserLookupResult{}, err
		}
		defer c.Release()
		var (
			name        string
			epoch       int32
			suspendedAt sql.NullTime
		)
		err = c.QueryRow(
			ctx,
			"SELECT username, session_epoch, suspended_at FROM users WHERE id = $1", id,
		).Scan(&name, &epoch, &suspendedAt)
		return middleware.UserLookupResult{
			Username:     name,
			SessionEpoch: epoch,
			IsSuspended:  suspendedAt.Valid,
		}, err
	}))
	csrf := middleware.CSRF(middleware.CSRFConfig{
		FailureHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "csrf: "+nosurfReason(r), http.StatusForbidden)
		}),
	})
	r.Group(func(r chi.Router) { apiH.Mount(r) })
	r.Group(func(r chi.Router) {
		r.Use(csrf)
		authH.Mount(r)
	})

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, pool, captor
}

func signupAndLoginFor(t *testing.T, srv *httptest.Server, pool *pgxpool.Pool, captor *captureSender, username string) (*client, int64) {
	t.Helper()
	cli := newClient(t, srv)
	mustSignup(t, cli, username, username+"@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()
	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {username},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	captor.reset()
	var id int64
	if err := pool.QueryRow(context.Background(),
		"SELECT id FROM users WHERE username = $1", username).Scan(&id); err != nil {
		t.Fatalf("lookup user %q: %v", username, err)
	}
	return cli, id
}

// createTokenForm posts a token create request with the given form values
// and returns the response body. It always succeeds at the HTTP layer (the
// handler renders the page with an error rather than returning 4xx for
// validation failures).
func createTokenForm(t *testing.T, cli *client, form url.Values) string {
	t.Helper()
	form.Set("csrf_token", cli.extractCSRF(t, "/settings/tokens"))
	resp := cli.post(t, "/settings/tokens", form)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return string(body)
}

// TestPATIPAllowlist_FreeReportOnlyAllows confirms that with the
// enforce flag off (the default), a Free user can attach an allowlist:
// the report-only path logs the would-deny but persists the row.
func TestPATIPAllowlist_FreeReportOnlyAllows(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{})
	cli, _ := signupAndLoginFor(t, srv, pool, captor, "alice11free")

	body := createTokenForm(t, cli, url.Values{
		"name":         {"ci"},
		"scopes":       {"user:read"},
		"ip_allowlist": {"203.0.113.0/24"},
	})
	if !strings.Contains(body, "RAW=shithub_pat_") {
		t.Fatalf("expected token to be minted in report-only mode, body=%s", body)
	}
}

// TestPATIPAllowlist_FreeEnforceRejects confirms that with the enforce
// flag on, a Free user attempting to attach an allowlist sees the
// upgrade banner instead of a successful mint.
func TestPATIPAllowlist_FreeEnforceRejects(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{
		UserFineGrainedPATs: true,
	})
	cli, _ := signupAndLoginFor(t, srv, pool, captor, "alice11enforce")

	body := createTokenForm(t, cli, url.Values{
		"name":         {"ci"},
		"scopes":       {"user:read"},
		"ip_allowlist": {"203.0.113.0/24"},
	})
	if strings.Contains(body, "RAW=shithub_pat_") {
		t.Fatalf("Free user with enforce on should NOT mint: %s", body)
	}
	if !strings.Contains(strings.ToLower(body), "pro") {
		t.Fatalf("rejection message should reference Pro upgrade: %s", body)
	}
}

// TestPATIPAllowlist_ProEnforceAllows confirms that a Pro user always
// gets the allowlist through, even with the enforce flag on.
func TestPATIPAllowlist_ProEnforceAllows(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{
		UserFineGrainedPATs: true,
	})
	cli, userID := signupAndLoginFor(t, srv, pool, captor, "alice11pro")
	upgradeProfileTestUserToPro(t, pool, userID)

	body := createTokenForm(t, cli, url.Values{
		"name":         {"ci"},
		"scopes":       {"user:read"},
		"ip_allowlist": {"203.0.113.0/24"},
	})
	if !strings.Contains(body, "RAW=shithub_pat_") {
		t.Fatalf("Pro user with enforce on should mint: %s", body)
	}
}

// TestPATIPAllowlist_MalformedRejected pins the validation contract:
// the handler refuses to mint a token if any allowlist entry can't be
// parsed, regardless of plan or enforce flag.
func TestPATIPAllowlist_MalformedRejected(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{})
	cli, _ := signupAndLoginFor(t, srv, pool, captor, "alice11bad")

	body := createTokenForm(t, cli, url.Values{
		"name":         {"ci"},
		"scopes":       {"user:read"},
		"ip_allowlist": {"203.0.113.0/24\nnot-an-ip"},
	})
	if strings.Contains(body, "RAW=shithub_pat_") {
		t.Fatalf("malformed allowlist must not mint: %s", body)
	}
	if !strings.Contains(strings.ToLower(body), "invalid") {
		t.Fatalf("error message should mention invalid entries: %s", body)
	}
}
