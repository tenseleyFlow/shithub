// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"database/sql"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/justinas/nosurf"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/password"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	authh "github.com/tenseleyFlow/shithub/internal/web/handlers/auth"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// captureSender records every Send call. Used by tests to assert what
// would have been emailed and to extract verification/reset tokens.
type captureSender struct {
	mu  sync.Mutex
	out []email.Message
}

func (c *captureSender) Send(_ context.Context, m email.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out = append(c.out, m)
	return nil
}

func (c *captureSender) all() []email.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]email.Message, len(c.out))
	copy(out, c.out)
	return out
}

func (c *captureSender) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out = nil
}

// fastArgon keeps tests under a few seconds. The full-cost defaults are
// exercised by the unit test in internal/auth/password.
var fastArgon = password.Params{Memory: 16 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func newTestServer(t *testing.T, requireVerify bool) (*httptest.Server, *captureSender) {
	srv, _, captor := newTestServerWithPool(t, requireVerify)
	return srv, captor
}

type authTestOptions struct {
	RequireVerify     bool
	OrgBillingEnabled bool
}

// newTestServerWithPool is identical to newTestServer but also exposes
// the underlying pool so tests that need to manipulate DB state (e.g.
// backdating timestamps) can do so against the SAME database the server
// is reading from. Use the simpler newTestServer when no DB poking is
// needed.
func newTestServerWithPool(t *testing.T, requireVerify bool) (*httptest.Server, *pgxpool.Pool, *captureSender) {
	return newTestServerWithPoolOptions(t, authTestOptions{RequireVerify: requireVerify})
}

func newTestServerWithPoolOptions(t *testing.T, opts authTestOptions) (*httptest.Server, *pgxpool.Pool, *captureSender) {
	t.Helper()
	pool := dbtest.NewTestDB(t)

	tmplFS := authTemplatesFS()
	rr, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}

	storeKey, err := session.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	store, err := session.NewCookieStore(session.CookieStoreConfig{
		Key: storeKey, MaxAge: 24 * time.Hour, Secure: false,
	})
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}

	cap := &captureSender{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	totpKey, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatalf("secretbox key: %v", err)
	}
	box, err := secretbox.FromBytes(totpKey)
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}

	h, err := authh.New(authh.Deps{
		Logger:       logger,
		Render:       rr,
		Pool:         pool,
		SessionStore: store,
		Email:        cap,
		Branding: email.Branding{
			SiteName: "shithub", BaseURL: "http://test.invalid",
			From: "noreply@shithub.test",
		},
		Argon2:                   fastArgon,
		Limiter:                  throttle.NewLimiter(),
		RequireEmailVerification: opts.RequireVerify,
		SecretBox:                box,
		ObjectStore:              storage.NewMemoryStore(),
		OrgBillingEnabled:        opts.OrgBillingEnabled,
	})
	if err != nil {
		t.Fatalf("authh.New: %v", err)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP(middleware.RealIPConfig{}))
	r.Use(middleware.SessionLoader(store, logger))
	r.Use(middleware.OptionalUser(func(ctx context.Context, id int64) (middleware.UserLookupResult, error) {
		// Cheap lookup against the test pool — settings handlers use the
		// username, and the epoch comparison enforces log-out-everywhere
		// across the same suite.
		u, err := pool.Acquire(ctx)
		if err != nil {
			return middleware.UserLookupResult{}, err
		}
		defer u.Release()
		var (
			name        string
			epoch       int32
			suspendedAt sql.NullTime
		)
		err = u.QueryRow(
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
	r.Group(func(r chi.Router) {
		r.Use(csrf)
		h.Mount(r)
	})

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, pool, cap
}

// authTemplatesFS returns a minimal templates FS sufficient for the auth
// handlers to render successfully. Each form wraps the CSRF token in
// `<<<CSRF:...:CSRF>>>` markers so the test client can extract it
// unambiguously regardless of the token's base64 alphabet.
func authTemplatesFS() fs.FS {
	layout := `{{ define "layout" }}<!DOCTYPE html><html><head><title>{{ .Title }}</title></head><body>{{ template "page" . }}</body></html>{{ end }}`
	signup := `{{ define "page" }}<form>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}<input name=username value="{{.Form.Username}}"><input name=csrf_token value="{{.CSRFToken}}"></form>{{ end }}`
	login := `{{ define "page" }}<form>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}{{ with .Notice }}<p class=notice>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}"></form>{{ end }}`
	resetReq := `{{ define "page" }}<form>{{ with .Notice }}<p class=notice>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}"></form>{{ end }}`
	resetConf := `{{ define "page" }}<form>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}">{{.Token}}</form>{{ end }}`
	verifyResend := `{{ define "page" }}<form>{{ with .Notice }}<p class=notice>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}"></form>{{ end }}`
	tfaChallenge := `{{ define "page" }}<form>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}"><input name=next value="{{.Next}}"></form>{{ end }}`
	tfaEnable := `{{ define "page" }}<form>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}">SECRET={{.Secret}}</form>{{ end }}`
	tfaDisable := `{{ define "page" }}<form>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}"></form>{{ end }}`
	tfaRecovery := `{{ define "page" }}<form>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}">{{ if .RecoveryCodes }}CODES={{ range .RecoveryCodes }}{{.}};{{ end }}{{ end }}</form>{{ end }}`
	keysTpl := `{{ define "page" }}<form>{{ with .AddError }}<p class=error>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}">KEYS={{ range .Keys }}{{.ID}}:{{.FingerprintSha256}};{{ end }}GPGKEYS={{ range .GPGKeys }}{{.ID}}:{{.Name}}:{{.KeyID}};{{ end }}</form>{{ end }}`
	//nolint:gosec // G101 false positive: test fixture, not a hardcoded credential.
	gpgAddTpl := `{{ define "page" }}<form action="/settings/keys/gpg" method=POST>{{ with .AddError }}<p class=error>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}"><input name=title value="{{.AddTitle}}"><textarea name=armored_key>{{.AddBlob}}</textarea></form>{{ end }}`
	//nolint:gosec // G101 false positive: test fixture, not a hardcoded credential.
	tokensTpl := `{{ define "page" }}<form>{{ with .CreateError }}<p class=error>{{.}}</p>{{ end }}<input name=csrf_token value="{{.CSRFToken}}">{{ if .JustCreatedRaw }}RAW={{.JustCreatedRaw}}{{ end }}TOKENS={{ range .Tokens }}{{.ID}}:{{.TokenPrefix}}{{ if .RevokedAt.Valid }}:revoked{{ end }};{{ end }}</form>{{ end }}`
	profileTpl := `{{ define "page" }}<h1>Public profile</h1>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}{{ with .Success }}<p class=notice>{{.}}</p>{{ end }}<form><input name=csrf_token value="{{.CSRFToken}}">DISPLAY={{.Form.DisplayName}};BIO={{.Form.Bio}};LOCATION={{.Form.Location}};WEBSITE={{.Form.Website}};COMPANY={{.Form.Company}};PRONOUNS={{.Form.Pronouns}};</form>{{ if .HasAvatar }}<form action="/settings/profile/avatar/remove" method=POST><input name=csrf_token value="{{.CSRFToken}}"><button>Remove</button></form>{{ end }}{{ end }}`
	accountTpl := `{{ define "page" }}<h1>Account</h1>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}{{ with .Success }}<p class=notice>{{.}}</p>{{ end }}<form action="/settings/account/username" method=POST><input name=csrf_token value="{{.CSRFToken}}">USERNAME={{.CurrentUsername}};USED={{.RecentRenames}}/{{.MaxRenames}};</form>{{ end }}`
	//nolint:gosec // G101 false positive: HTML fixture, not a credential.
	pwTpl := `{{ define "page" }}<h1>Password</h1>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}{{ with .Success }}<p class=notice>{{.}}</p>{{ end }}<form action="/settings/password" method=POST><input name=csrf_token value="{{.CSRFToken}}">RECENT={{.RecentAuthOK}};</form>{{ end }}`
	apprTpl := `{{ define "page" }}<h1>Appearance</h1>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}{{ with .Success }}<p class=notice>{{.}}</p>{{ end }}<form action="/settings/appearance" method=POST><input name=csrf_token value="{{.CSRFToken}}">THEME={{.CurrentTheme}};</form>{{ end }}`
	emailsTpl := `{{ define "page" }}<h1>Emails</h1>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}{{ with .Success }}<p class=notice>{{.}}</p>{{ end }}<form action="/settings/emails" method=POST><input name=csrf_token value="{{.CSRFToken}}"></form>EMAILS={{ range .Emails }}{{.ID}}:{{.Email}}:p={{.IsPrimary}}:v={{.Verified}};{{ end }}{{ end }}`
	notifTpl := `{{ define "page" }}<h1>Notifications</h1>{{ with .Success }}<p class=notice>{{.}}</p>{{ end }}<form action="/settings/notifications" method=POST><input name=csrf_token value="{{.CSRFToken}}">CHANNELS={{ range .Channels }}{{.Key}}:e={{.Enabled}}:r={{.Required}};{{ end }}</form>{{ end }}`
	organizationsTpl := `{{ define "page" }}<h1>Organizations</h1>USER={{.Username}};ORGS={{ range .Organizations }}{{.Slug}}:{{.RoleLabel}}:manage={{.CanManage}}:compare={{.CompareHref}};{{ end }}{{ end }}`
	sessTpl := `{{ define "page" }}<h1>Sessions</h1>{{ with .Success }}<p class=notice>{{.}}</p>{{ end }}<form action="/settings/sessions/logout-everywhere" method=POST><input name=csrf_token value="{{.CSRFToken}}">UA={{.UserAgent}};</form>{{ end }}`
	dangerTpl := `{{ define "page" }}<h1>Delete</h1>{{ with .Error }}<p class=error>{{.}}</p>{{ end }}<form action="/settings/danger" method=POST><input name=csrf_token value="{{.CSRFToken}}">USER={{.Username}};GRACE={{.GraceWindowDays}};</form>{{ end }}`
	errorPage := `{{ define "page" }}<h1>{{.Status}} {{.StatusText}}</h1><p>{{.Message}}</p>{{ end }}`
	return fstest.MapFS{
		"_layout.html":                {Data: []byte(layout)},
		"hello.html":                  {Data: []byte(`{{ define "page" }}home{{ end }}`)},
		"auth/signup.html":            {Data: []byte(signup)},
		"auth/login.html":             {Data: []byte(login)},
		"auth/reset_request.html":     {Data: []byte(resetReq)},
		"auth/reset_confirm.html":     {Data: []byte(resetConf)},
		"auth/verify_resend.html":     {Data: []byte(verifyResend)},
		"auth/2fa_challenge.html":     {Data: []byte(tfaChallenge)},
		"settings/2fa_enable.html":    {Data: []byte(tfaEnable)},
		"settings/2fa_disable.html":   {Data: []byte(tfaDisable)},
		"settings/2fa_recovery.html":  {Data: []byte(tfaRecovery)},
		"settings/keys.html":          {Data: []byte(keysTpl)},
		"settings/keys_gpg_add.html":  {Data: []byte(gpgAddTpl)},
		"settings/tokens.html":        {Data: []byte(tokensTpl)},
		"settings/profile.html":       {Data: []byte(profileTpl)},
		"settings/account.html":       {Data: []byte(accountTpl)},
		"settings/password.html":      {Data: []byte(pwTpl)},
		"settings/appearance.html":    {Data: []byte(apprTpl)},
		"settings/emails.html":        {Data: []byte(emailsTpl)},
		"settings/notifications.html": {Data: []byte(notifTpl)},
		"settings/organizations.html": {Data: []byte(organizationsTpl)},
		"settings/sessions.html":      {Data: []byte(sessTpl)},
		"settings/danger.html":        {Data: []byte(dangerTpl)},
		"errors/404.html":             {Data: []byte(errorPage)},
		"errors/403.html":             {Data: []byte(errorPage)},
		"errors/429.html":             {Data: []byte(errorPage)},
		"errors/500.html":             {Data: []byte(errorPage)},
	}
}

// client wraps http.Client with a cookie jar so session/CSRF cookies persist.
type client struct {
	c   *http.Client
	srv *httptest.Server
}

func newClient(t *testing.T, srv *httptest.Server) *client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &client{
		c: &http.Client{
			Jar: jar,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		srv: srv,
	}
}

func (c *client) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := c.c.Get(c.srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func (c *client) post(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	// nosurf enforces same-origin on POST via Origin/Referer (browsers set
	// these for form submissions; http.Client.PostForm does not).
	req, err := http.NewRequest(http.MethodPost, c.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", c.srv.URL+path)
	resp, err := c.c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// extractCSRF GETs path and returns the CSRF token the form would carry.
// The CSRF middleware sets the token cookie on first GET; the test
// templates wrap the printed token in `<<<CSRF:...:CSRF>>>` markers so
// extraction is unambiguous (nosurf uses base64 with `+/=` characters
// that a generic alphanumeric regex would mishandle).
func (c *client) extractCSRF(t *testing.T, path string) string {
	t.Helper()
	resp := c.get(t, path)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	m := csrfMarkerRE.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("no CSRF marker in body of %s: %s", path, body)
	}
	// html/template HTML-escapes `+` to `&#43;` (and similar) in attribute
	// values; browsers decode these transparently when reading form values,
	// so the test client must mirror that decoding.
	return html.UnescapeString(m[1])
}

var csrfMarkerRE = regexp.MustCompile(`name=csrf_token value="([^"]*)"`)

func nosurfReason(r *http.Request) string {
	if err := nosurf.Reason(r); err != nil {
		return err.Error()
	}
	return "no reason"
}

// extractTokenFromMessage pulls the URL-encoded token out of a verify or
// reset email body. The link shape is /<path>/<token>, where token is the
// b64url-encoded 32-byte payload from internal/auth/token.
func extractTokenFromMessage(t *testing.T, m email.Message, prefix string) string {
	t.Helper()
	re := regexp.MustCompile(prefix + `/([A-Za-z0-9_\-]{30,})`)
	for _, body := range []string{m.Text, m.HTML} {
		if mm := re.FindStringSubmatch(body); mm != nil {
			return mm[1]
		}
	}
	t.Fatalf("no token in message under prefix %s\nbodies:\n%s\n%s", prefix, m.Text, m.HTML)
	return ""
}

// ============================== tests ==================================

func TestSignup_Verify_Login_Logout(t *testing.T) {
	t.Parallel()
	srv, sender := newTestServer(t, true)
	cli := newClient(t, srv)

	csrf := cli.extractCSRF(t, "/signup")
	resp := cli.post(t, "/signup", url.Values{
		"csrf_token": {csrf},
		"username":   {"alice"},
		"email":      {"alice@example.com"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("signup: status %d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// login while unverified should be rejected.
	csrf = cli.extractCSRF(t, "/login")
	resp = cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"alice"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unverified login: status %d body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "verify your email") {
		t.Fatalf("expected verify-required message, got: %s", body)
	}
	_ = resp.Body.Close()

	// Use the captured email's token to verify.
	msgs := sender.all()
	if len(msgs) == 0 {
		t.Fatal("expected verification email")
	}
	tok := extractTokenFromMessage(t, msgs[0], "/verify-email")

	resp = cli.get(t, "/verify-email/"+tok)
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("verify: status %d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// Now log in successfully.
	csrf = cli.extractCSRF(t, "/login")
	resp = cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"alice"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("verified login: status %d body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/explore" {
		t.Fatalf("login redirect: %q, want /explore", loc)
	}
	_ = resp.Body.Close()

	// Logout — POST /logout with CSRF.
	csrf = cli.extractCSRF(t, "/login")
	resp = cli.post(t, "/logout", url.Values{"csrf_token": {csrf}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: status %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestLogin_AcceptsEmailAddress(t *testing.T) {
	t.Parallel()
	srv, sender := newTestServer(t, true)
	cli := newClient(t, srv)

	mustSignup(t, cli, "emailuser", "emailuser@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, sender.all()[0], "/verify-email")
	resp := cli.get(t, "/verify-email/"+tok)
	_ = resp.Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp = cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"EmailUser@Example.com"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("email login: status %d body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/explore" {
		t.Fatalf("email login redirect: %q, want /explore", loc)
	}
	_ = resp.Body.Close()
}

func TestPasswordReset_EndToEnd(t *testing.T) {
	t.Parallel()
	srv, sender := newTestServer(t, false)
	cli := newClient(t, srv)

	// Seed a verified user.
	mustSignup(t, cli, "bob", "bob@example.com", "original-password-1")
	tok := extractTokenFromMessage(t, sender.all()[0], "/verify-email")
	resp := cli.get(t, "/verify-email/"+tok)
	_ = resp.Body.Close()

	// Request a reset.
	csrf := cli.extractCSRF(t, "/password/reset")
	resp = cli.post(t, "/password/reset", url.Values{
		"csrf_token": {csrf},
		"email":      {"bob@example.com"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset request: status %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	all := sender.all()
	resetTok := extractTokenFromMessage(t, all[len(all)-1], "/password/reset")

	// Confirm.
	csrf = cli.extractCSRF(t, "/password/reset/"+resetTok)
	resp = cli.post(t, "/password/reset/"+resetTok, url.Values{
		"csrf_token": {csrf},
		"password":   {"brand-new-password-2"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("reset confirm: status %d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// Sign in with the new password.
	csrf = cli.extractCSRF(t, "/login")
	resp = cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"bob"},
		"password":   {"brand-new-password-2"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("post-reset login: status %d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()
}

func TestPasswordReset_UnknownEmail_GenericResponse(t *testing.T) {
	t.Parallel()
	srv, sender := newTestServer(t, false)
	cli := newClient(t, srv)

	csrf := cli.extractCSRF(t, "/password/reset")
	resp := cli.post(t, "/password/reset", url.Values{
		"csrf_token": {csrf},
		"email":      {"nobody@nowhere.example"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset for unknown: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "If an account is registered") {
		t.Fatalf("expected generic notice, got: %s", body)
	}
	if len(sender.all()) != 0 {
		t.Fatalf("expected no email sent for unknown address, got %d", len(sender.all()))
	}
}

func TestLogin_BruteForceThrottled(t *testing.T) {
	t.Parallel()
	srv, sender := newTestServer(t, false)
	cli := newClient(t, srv)

	mustSignup(t, cli, "carol", "carol@example.com", "original-password-1")
	tok := extractTokenFromMessage(t, sender.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	for i := 0; i < 6; i++ {
		csrf := cli.extractCSRF(t, "/login")
		resp := cli.post(t, "/login", url.Values{
			"csrf_token": {csrf},
			"username":   {"carol"},
			"password":   {"wrong-password"},
		})
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("attempt %d: status %d body=%s", i+1, resp.StatusCode, body)
		}
		_ = resp.Body.Close()
	}

	// 7th attempt should be throttled.
	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"carol"},
		"password":   {"wrong-password"},
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("7th attempt: status %d, want 429; body=%s", resp.StatusCode, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Fatal("missing Retry-After on throttled response")
	}
	_ = resp.Body.Close()
}

func TestLogin_ConstantTime(t *testing.T) {
	t.Parallel()
	srv, sender := newTestServer(t, false)
	cli := newClient(t, srv)

	mustSignup(t, cli, "dave", "dave@example.com", "original-password-1")
	tok := extractTokenFromMessage(t, sender.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	const trials = 10
	measure := func(username string) time.Duration {
		var total time.Duration
		for i := 0; i < trials; i++ {
			cli2 := newClient(t, srv)
			csrf := cli2.extractCSRF(t, "/login")
			start := time.Now()
			resp := cli2.post(t, "/login", url.Values{
				"csrf_token": {csrf},
				"username":   {username},
				"password":   {"any-wrong-password"},
			})
			total += time.Since(start)
			_ = resp.Body.Close()
		}
		return total / trials
	}
	existing := measure("dave")
	missing := measure("does-not-exist")
	delta := existing - missing
	if delta < 0 {
		delta = -delta
	}
	// On the test argon params (~5–15ms) any user-existence shortcut would
	// shave off most of the time; allow generous slack for CI noise but
	// reject a 5x divergence.
	if existing > missing*5 || missing > existing*5 {
		t.Fatalf("login timing diverges too much: existing=%v missing=%v delta=%v", existing, missing, delta)
	}
}

func TestSignup_ReservedNameRejected(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, false)
	cli := newClient(t, srv)

	csrf := cli.extractCSRF(t, "/signup")
	resp := cli.post(t, "/signup", url.Values{
		"csrf_token": {csrf},
		"username":   {"login"},
		"email":      {"x@example.com"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 with form re-render", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "reserved") {
		t.Fatalf("expected reserved-name error, got: %s", body)
	}
}

func TestSignup_CommonPasswordRejected(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, false)
	cli := newClient(t, srv)

	csrf := cli.extractCSRF(t, "/signup")
	resp := cli.post(t, "/signup", url.Values{
		"csrf_token": {csrf},
		"username":   {"erin"},
		"email":      {"erin@example.com"},
		"password":   {"qwertyuiop"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "common") {
		t.Fatalf("expected common-password error, got: %s", body)
	}
}

func TestSignup_HoneypotSilent(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, false)
	cli := newClient(t, srv)
	csrf := cli.extractCSRF(t, "/signup")
	resp := cli.post(t, "/signup", url.Values{
		"csrf_token": {csrf},
		"username":   {"frank"},
		"email":      {"frank@example.com"},
		"password":   {"correct horse battery staple"},
		"company":    {"oops, a bot filled this"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("honeypot: expected redirect, got %d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()
}

// mustSignup is a convenience for tests that need a seeded user.
func mustSignup(t *testing.T, cli *client, username, em, pw string) {
	t.Helper()
	csrf := cli.extractCSRF(t, "/signup")
	resp := cli.post(t, "/signup", url.Values{
		"csrf_token": {csrf},
		"username":   {username},
		"email":      {em},
		"password":   {pw},
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("seed signup %s: status %d body=%s", username, resp.StatusCode, body)
	}
}
