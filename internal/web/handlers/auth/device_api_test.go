// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/devicecode"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/password"
	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	authh "github.com/tenseleyFlow/shithub/internal/web/handlers/auth"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// newDeviceCodeRouter wires only the JSON device-code endpoints on a
// bare router (no CSRF, no session loader) — these endpoints are
// always CSRF-exempt in production. We use a minimal templates FS
// because the JSON handlers never render HTML.
func newDeviceCodeRouter(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	tmplFS := fstest.MapFS{
		"_layout.html": {Data: []byte(`{{ define "layout" }}{{ template "page" . }}{{ end }}`)},
		"hello.html":   {Data: []byte(`{{ define "page" }}home{{ end }}`)},
	}
	rr, err := render.New(fs.FS(tmplFS), render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	storeKey, err := session.GenerateKey()
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	store, err := session.NewCookieStore(session.CookieStoreConfig{Key: storeKey, MaxAge: 0, Secure: false})
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}
	h, err := authh.New(authh.Deps{
		Logger:       slog.Default(),
		Render:       rr,
		Pool:         pool,
		SessionStore: store,
		Email:        &noopSender{},
		Branding: email.Branding{
			SiteName: "shithub", BaseURL: "http://test.invalid",
			From: "noreply@shithub.test",
		},
		Argon2: password.Params{
			Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32,
		},
		Limiter: throttle.NewLimiter(),
		Audit:   audit.NewRecorder(),
		DeviceCode: devicecode.Config{
			ClientIDs:     []string{"shithub-cli"},
			DefaultScopes: []string{"user:read"},
		},
	})
	if err != nil {
		t.Fatalf("authh.New: %v", err)
	}
	r := chi.NewRouter()
	h.MountDeviceCodeAPI(r)
	return r, pool
}

type noopSender struct{}

func (noopSender) Send(ctx context.Context, msg email.Message) error { return nil }

// seedDeviceUser inserts a user row that the Approve path can stamp
// as the authorizer. The hash is a constant argon2id digest; we don't
// log this user in via the password flow so the value doesn't have to
// match anything meaningful.
const seedDeviceUserHash = "$argon2id$v=19$m=1024,t=1,p=1$YWFhYWFhYWFhYWFhYWFhYQ$" +
	"DvBOTSnFhCBe+Pfx/W7Sk3hG3JCm2Wj0RBgCu+CPDtY"

func seedDeviceUser(t *testing.T, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	q := usersdb.New()
	u, err := q.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username:     username,
		DisplayName:  strings.ToUpper(username[:1]) + username[1:],
		PasswordHash: seedDeviceUserHash,
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

func formPost(router http.Handler, path string, body url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

type oauthErr struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type deviceIssue struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type deviceExchange struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

func TestDeviceAPI_IssueShape(t *testing.T) {
	router, _ := newDeviceCodeRouter(t)
	rr := formPost(router, "/login/device/code", url.Values{
		"client_id": {"shithub-cli"},
		"scope":     {"user:read repo:read"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var out deviceIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		t.Fatalf("empty codes: %+v", out)
	}
	if out.VerificationURI != "http://test.invalid/login/device" {
		t.Errorf("verification_uri: got %q", out.VerificationURI)
	}
	if !strings.Contains(out.VerificationURIComplete, "user_code=") {
		t.Errorf("verification_uri_complete missing user_code: %q", out.VerificationURIComplete)
	}
	if out.ExpiresIn <= 0 || out.Interval <= 0 {
		t.Errorf("expiry/interval not populated: %+v", out)
	}
}

func TestDeviceAPI_IssueRejectsUnknownClient(t *testing.T) {
	router, _ := newDeviceCodeRouter(t)
	rr := formPost(router, "/login/device/code", url.Values{"client_id": {"evil-cli"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var oe oauthErr
	_ = json.Unmarshal(rr.Body.Bytes(), &oe)
	if oe.Error != "unauthorized_client" {
		t.Errorf("error code: got %q", oe.Error)
	}
}

func TestDeviceAPI_ExchangeRejectsWrongGrantType(t *testing.T) {
	router, _ := newDeviceCodeRouter(t)
	rr := formPost(router, "/login/oauth/access_token", url.Values{
		"grant_type": {"authorization_code"},
		"client_id":  {"shithub-cli"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var oe oauthErr
	_ = json.Unmarshal(rr.Body.Bytes(), &oe)
	if oe.Error != "unsupported_grant_type" {
		t.Errorf("error: got %q", oe.Error)
	}
}

func TestDeviceAPI_ExchangePendingThenApproved(t *testing.T) {
	router, pool := newDeviceCodeRouter(t)
	userID := seedDeviceUser(t, pool, "alice")

	// 1) Issue a code via HTTP.
	rr := formPost(router, "/login/device/code", url.Values{
		"client_id": {"shithub-cli"},
		"scope":     {"user:read"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("issue status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var issued deviceIssue
	_ = json.Unmarshal(rr.Body.Bytes(), &issued)

	// 2) Pending poll → 400 authorization_pending.
	const exchangeGrant = "urn:ietf:params:oauth:grant-type:device_code"
	rr = formPost(router, "/login/oauth/access_token", url.Values{
		"grant_type":  {exchangeGrant},
		"client_id":   {"shithub-cli"},
		"device_code": {issued.DeviceCode},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("pending status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var oe oauthErr
	_ = json.Unmarshal(rr.Body.Bytes(), &oe)
	if oe.Error != "authorization_pending" {
		t.Errorf("pending error: got %q", oe.Error)
	}

	// 3) Approve directly via the orchestrator (HTML form flow would do
	// this via the verification page; we shortcut here since this test
	// is about the JSON exchange path).
	row, err := devicecode.LookupByUserCode(context.Background(), devicecode.Deps{Pool: pool}, issued.UserCode)
	if err != nil {
		t.Fatalf("LookupByUserCode: %v", err)
	}
	if err := devicecode.Approve(context.Background(), devicecode.Deps{Pool: pool}, row.ID, userID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Rewind last_polled_at so the slow_down gate doesn't bite the next exchange.
	if _, err := pool.Exec(context.Background(),
		"UPDATE device_authorizations SET last_polled_at = now() - interval '10 seconds' WHERE id = $1",
		row.ID); err != nil {
		t.Fatalf("rewind last_polled_at: %v", err)
	}

	// 4) Exchange after approval → 200 + access_token.
	rr = formPost(router, "/login/oauth/access_token", url.Values{
		"grant_type":  {exchangeGrant},
		"client_id":   {"shithub-cli"},
		"device_code": {issued.DeviceCode},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("exchange status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var ex deviceExchange
	_ = json.Unmarshal(rr.Body.Bytes(), &ex)
	if !strings.HasPrefix(ex.AccessToken, "shithub_pat_") {
		t.Errorf("access_token: got %q", ex.AccessToken)
	}
	if ex.TokenType != "bearer" {
		t.Errorf("token_type: got %q", ex.TokenType)
	}
	if ex.Scope != "user:read" {
		t.Errorf("scope: got %q", ex.Scope)
	}
}
