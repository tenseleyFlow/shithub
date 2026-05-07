// SPDX-License-Identifier: AGPL-3.0-or-later

package profile_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	profileh "github.com/tenseleyFlow/shithub/internal/web/handlers/profile"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

type profileEnv struct {
	srv  *httptest.Server
	pool *pgxpool.Pool
	q    *usersdb.Queries
}

func setupProfileEnv(t *testing.T) *profileEnv {
	t.Helper()
	pool := dbtest.NewTestDB(t)

	tmplFS := fstest.MapFS{
		"_layout.html":           {Data: []byte(`{{ define "layout" }}<html><head><title>{{ .Title }}</title></head><body>{{ template "page" . }}</body></html>{{ end }}`)},
		"hello.html":             {Data: []byte(`{{ define "page" }}home{{ end }}`)},
		"profile/view.html":      {Data: []byte(`{{ define "page" }}USER={{.User.Username}} DISPLAY={{.User.DisplayName}}{{ if .IsSelf }} SELF=1{{ end }} BIO={{.User.Bio}}{{ end }}`)},
		"profile/suspended.html": {Data: []byte(`{{ define "page" }}SUSPENDED={{.Username}}{{ end }}`)},
		"errors/404.html":        {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":        {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}
	rr, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}

	h, err := profileh.New(profileh.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render: rr, Pool: pool,
	})
	if err != nil {
		t.Fatalf("profileh.New: %v", err)
	}

	r := chi.NewRouter()
	r.Get("/login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("login-handler"))
	})
	h.MountAvatars(r)
	h.MountProfile(r)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &profileEnv{srv: srv, pool: pool, q: usersdb.New()}
}

// fixtureHash is a static PHC test fixture (zero salt, zero key) — not a real credential.
const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func (e *profileEnv) insertUser(t *testing.T, username, display, bio string) usersdb.User {
	t.Helper()
	ctx := context.Background()
	user, err := e.q.CreateUser(ctx, e.pool, usersdb.CreateUserParams{
		Username:     username,
		DisplayName:  display,
		PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if bio != "" {
		if _, err := e.pool.Exec(ctx, "UPDATE users SET bio = $1 WHERE id = $2", bio, user.ID); err != nil {
			t.Fatalf("set bio: %v", err)
		}
	}
	return user
}

func (e *profileEnv) insertRedirect(t *testing.T, oldname string, userID int64) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		"INSERT INTO username_redirects (old_username, user_id) VALUES ($1, $2)",
		oldname, userID); err != nil {
		t.Fatalf("redirect insert: %v", err)
	}
}

func (e *profileEnv) suspend(t *testing.T, userID int64) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		"UPDATE users SET suspended_at = now(), suspended_reason = 'test' WHERE id = $1",
		userID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
}

func newNonRedirClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("jar: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// =============================== tests ==================================

func TestProfile_RendersForExistingUser(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	env.insertUser(t, "alice", "Alice Anderson", "Hi.")

	resp, err := newNonRedirClient(t).Get(env.srv.URL + "/alice")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"USER=alice", "DISPLAY=Alice Anderson", "BIO=Hi."} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q in body: %s", want, body)
		}
	}
}

func TestProfile_UnknownUser404(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	resp, _ := http.Get(env.srv.URL + "/no-such-user")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

func TestProfile_CasingRedirect(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	env.insertUser(t, "alice", "Alice", "")
	resp, err := newNonRedirClient(t).Get(env.srv.URL + "/Alice")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status %d, want 301", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "/alice" {
		t.Fatalf("Location = %q", resp.Header.Get("Location"))
	}
}

func TestProfile_UsernameRedirect(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	user := env.insertUser(t, "alice", "Alice", "")
	env.insertRedirect(t, "oldname", user.ID)

	resp, err := newNonRedirClient(t).Get(env.srv.URL + "/oldname")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "/alice" {
		t.Fatalf("Location = %q", resp.Header.Get("Location"))
	}
}

func TestProfile_SuspendedRendersUnavailable(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	user := env.insertUser(t, "badactor", "Bad", "")
	env.suspend(t, user.ID)

	resp, _ := http.Get(env.srv.URL + "/badactor")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status %d, want 410", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "SUSPENDED=badactor") {
		t.Fatalf("expected suspended template, got: %s", body)
	}
}

func TestProfile_ReservedNameNotShadowed(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	resp, _ := http.Get(env.srv.URL + "/login")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "login-handler" {
		t.Fatalf("expected login-handler, got %q", body)
	}
}

func TestProfile_ReservedShortcircuit(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	resp, _ := http.Get(env.srv.URL + "/admin")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reserved path status: %d, want 404", resp.StatusCode)
	}
}

func TestProfile_AvatarReturnsIdenticonForNoKey(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	env.insertUser(t, "alice", "Alice", "")
	resp, _ := http.Get(env.srv.URL + "/avatars/alice")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("content-type %q", resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<svg") {
		t.Fatalf("expected svg in body")
	}
}

// TestReservedNameList_HasReasonableContents is the route-audit test: it
// asserts every top-level path segment shithub registers as of S09 is on
// the reserved list. When a future sprint adds a new top-level route,
// this test fails until reserved.go is updated.
func TestReservedNameList_HasReasonableContents(t *testing.T) {
	t.Parallel()
	mustReserved := []string{
		"signup", "login", "logout",
		"password", "verify-email",
		"settings", "api", "admin",
		"static", "healthz", "readyz", "metrics",
		"keys", "tokens",
		"avatars",
	}
	for _, n := range mustReserved {
		if !authpkg.IsReserved(n) {
			t.Errorf("expected %q to be on the reserved list", n)
		}
	}
}
