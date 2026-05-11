// SPDX-License-Identifier: AGPL-3.0-or-later

package profile_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	profileh "github.com/tenseleyFlow/shithub/internal/web/handlers/profile"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

type profileEnv struct {
	srv    *httptest.Server
	pool   *pgxpool.Pool
	q      *usersdb.Queries
	repoFS *storage.RepoFS
}

func setupProfileEnv(t *testing.T) *profileEnv {
	return setupProfileEnvWithStore(t, nil)
}

func setupProfileEnvWithStore(t *testing.T, objectStore storage.ObjectStore) *profileEnv {
	return setupProfileEnvWithDeps(t, objectStore, nil)
}

func setupProfileEnvWithRepoFS(t *testing.T) *profileEnv {
	t.Helper()
	repoFS, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	return setupProfileEnvWithDeps(t, nil, repoFS)
}

func setupProfileEnvWithDeps(t *testing.T, objectStore storage.ObjectStore, repoFS *storage.RepoFS) *profileEnv {
	t.Helper()
	pool := dbtest.NewTestDB(t)

	tmplFS := fstest.MapFS{
		"_layout.html":             {Data: []byte(`{{ define "layout" }}<html><head><title>{{ .Title }}</title></head><body>{{ template "page" . }}</body></html>{{ end }}`)},
		"hello.html":               {Data: []byte(`{{ define "page" }}home{{ end }}`)},
		"profile/view.html":        {Data: []byte(`{{ define "page" }}USER={{.User.Username}} DISPLAY={{.User.DisplayName}}{{ if .IsSelf }} SELF=1{{ end }}{{ if .IsFollowing }} FOLLOWING=1{{ end }} FOLLOWERS={{.FollowersCount}} FOLLOWINGCOUNT={{.FollowingCount}} BIO={{.User.Bio}} VISIBLE={{.VisibleRepoCount}} ORGS={{len .Orgs}} README={{.HasProfileReadme}} CONTRIB={{.Contributions.Total}} PERIOD={{.Contributions.Period}} PRIVATE={{.Contributions.IncludePrivateContributions}} WEEKS={{len .Contributions.Weeks}} YEARS={{len .Contributions.Years}} YEARLINKS={{range .Contributions.Years}}{{.Year}}:{{.Active}}:{{.Href}};{{end}} PINS={{len .PinnedRepos}} PINNAMES={{range .PinnedRepos}}{{.Name}};{{end}} CANDIDATES={{len .PinCandidates}} SELECTED={{range .PinCandidates}}{{if .IsPinned}}{{.Name}};{{end}}{{end}}{{ if .CanCustomizePins }} CUSTOMIZE=1 ACTION={{.ContributionSettingsAction}} RETURN={{.ContributionSettingsReturn}}{{ end }}{{ end }}`)},
		"profile/follows_tab.html": {Data: []byte(`{{ define "page" }}FOLLOWTAB={{.ActiveTab}} USER={{.User.Username}} TOTAL={{len .Items}} ITEMS={{range .Items}}{{.Kind}}:{{.Username}};{{end}}{{ end }}`)},
		"profile/suspended.html":   {Data: []byte(`{{ define "page" }}SUSPENDED={{.Username}}{{ end }}`)},
		"orgs/profile.html":        {Data: []byte(`{{ define "page" }}ORG={{.Org.Slug}}{{ if .IsFollowing }} FOLLOWING=1{{ end }} FOLLOWERS={{.FollowerCount}} REPOS={{len .Repos}} PINS={{len .PinnedRepos}} PINNAMES={{range .PinnedRepos}}{{.Name}};{{end}} CANDIDATES={{len .PinCandidates}} SELECTED={{range .PinCandidates}}{{if .IsPinned}}{{.Name}};{{end}}{{end}} MEMBERS={{.MemberCount}} PEOPLE={{len .People}} NAMES={{range .Repos}}{{.Name}};{{end}} LANGS={{range .TopLanguages}}{{.Name}}={{.Count}};{{end}} TOPICS={{range .TopTopics}}{{.Name}}={{.Count}};{{end}} VIEWAS={{.ViewAs}}{{ if .CanCustomizePins }} CUSTOMIZE=1{{ end }}{{ end }}`)},
		"orgs/repositories.html":   {Data: []byte(`{{ define "page" }}ORGREPOS={{.Org.Slug}} ACTIVE={{.ActiveOrgNav}} TOTAL={{.RepoCount}} FILTERED={{.FilteredCount}} PAGE={{.Page}}/{{.PageCount}} TYPE={{.SelectedType}} LANG={{.SelectedLanguage}} SORT={{.SelectedSort}} PREV={{.PrevHref}} NEXT={{.NextHref}} NAMES={{range .Repos}}{{.Name}};{{end}}{{range .PaginationPages}} P{{.Number}}={{.Current}}{{end}}{{ end }}`)},
		"errors/404.html":          {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":          {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}
	rr, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}

	h, err := profileh.New(profileh.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render: rr, Pool: pool,
		RepoFS:      repoFS,
		ObjectStore: objectStore,
	})
	if err != nil {
		t.Fatalf("profileh.New: %v", err)
	}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawID := r.Header.Get("X-Test-User-ID")
			if rawID == "" {
				next.ServeHTTP(w, r)
				return
			}
			id, err := strconv.ParseInt(rawID, 10, 64)
			if err != nil {
				t.Fatalf("bad X-Test-User-ID: %v", err)
			}
			u := middleware.CurrentUser{
				ID:       id,
				Username: r.Header.Get("X-Test-Username"),
			}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), u)))
		})
	})
	r.Get("/login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("login-handler"))
	})
	h.MountAvatars(r)
	h.MountOrgRepositories(r)
	h.MountProfile(r)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &profileEnv{srv: srv, pool: pool, q: usersdb.New(), repoFS: repoFS}
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

func (e *profileEnv) insertVerifiedEmail(t *testing.T, userID int64, email string) {
	t.Helper()
	if _, err := e.q.CreateUserEmail(context.Background(), e.pool, usersdb.CreateUserEmailParams{
		UserID:    userID,
		Email:     email,
		IsPrimary: true,
		Verified:  true,
	}); err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
}

func (e *profileEnv) insertOrg(t *testing.T, slug, display, desc string, creator usersdb.User) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID int64
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, display_name, description, created_by_user_id)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		slug, display, desc, creator.ID).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := e.pool.Exec(ctx,
		`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, creator.ID); err != nil {
		t.Fatalf("insert org member: %v", err)
	}
	return orgID
}

func (e *profileEnv) insertOrgRepo(t *testing.T, orgID int64, name, desc, visibility, language string, stars, forks int64, topics ...string) int64 {
	t.Helper()
	ctx := context.Background()
	var repoID int64
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO repos (
		    owner_org_id, name, description, visibility, default_branch,
		    primary_language, star_count, fork_count, updated_at
		  )
		  VALUES ($1, $2, $3, $4, 'trunk', $5, $6, $7, now())
		  RETURNING id`,
		orgID, name, desc, visibility, language, stars, forks).Scan(&repoID); err != nil {
		t.Fatalf("insert org repo: %v", err)
	}
	for _, topic := range topics {
		if _, err := e.pool.Exec(ctx,
			`INSERT INTO repo_topics (repo_id, topic) VALUES ($1, $2)`,
			repoID, topic); err != nil {
			t.Fatalf("insert topic: %v", err)
		}
	}
	return repoID
}

func (e *profileEnv) insertUserRepo(t *testing.T, userID int64, name, desc, visibility, language string, stars, forks int64) int64 {
	t.Helper()
	var repoID int64
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO repos (
		    owner_user_id, name, description, visibility, default_branch,
		    primary_language, star_count, fork_count, updated_at
		  )
		  VALUES ($1, $2, $3, $4, 'trunk', $5, $6, $7, now())
		  RETURNING id`,
		userID, name, desc, visibility, language, stars, forks).Scan(&repoID); err != nil {
		t.Fatalf("insert user repo: %v", err)
	}
	return repoID
}

func (e *profileEnv) writeInitialCommit(t *testing.T, owner, repoName, authorName, authorEmail string, when time.Time) string {
	t.Helper()
	if e.repoFS == nil {
		t.Fatal("repoFS not configured")
	}
	ctx := context.Background()
	gitDir, err := e.repoFS.RepoPath(owner, repoName)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := e.repoFS.InitBare(ctx, gitDir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	oid, err := repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		Message:     "Initial commit",
		Branch:      "trunk",
		When:        when,
		Files: []repogit.FileEntry{{
			Path: "README.md",
			Body: []byte("# " + repoName + "\n"),
		}},
	}.Build(ctx)
	if err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	return oid
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

func (e *profileEnv) getAs(t *testing.T, path string, user usersdb.User) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if user.ID != 0 {
		req.Header.Set("X-Test-User-ID", strconv.FormatInt(user.ID, 10))
		req.Header.Set("X-Test-Username", user.Username)
	}
	resp, err := newNonRedirClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func (e *profileEnv) postPins(t *testing.T, path string, user usersdb.User, repoIDs ...int64) *http.Response {
	t.Helper()
	form := url.Values{}
	for _, repoID := range repoIDs {
		form.Add("repo_id", strconv.FormatInt(repoID, 10))
	}
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Test-User-ID", strconv.FormatInt(user.ID, 10))
	req.Header.Set("X-Test-Username", user.Username)
	resp, err := newNonRedirClient(t).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (e *profileEnv) postContributionSettings(t *testing.T, path string, user usersdb.User, includePrivate bool, returnTo string) *http.Response {
	t.Helper()
	include := "0"
	if includePrivate {
		include = "1"
	}
	form := url.Values{
		"include_private_contributions": {include},
		"return_to":                     {returnTo},
	}
	return e.postFormAs(t, path, user, form)
}

func (e *profileEnv) postFormAs(t *testing.T, path string, user usersdb.User, form url.Values) *http.Response {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if user.ID != 0 {
		req.Header.Set("X-Test-User-ID", strconv.FormatInt(user.ID, 10))
		req.Header.Set("X-Test-Username", user.Username)
	}
	resp, err := newNonRedirClient(t).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
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

func TestProfile_FollowUserRoutesUpdateCountsAndState(t *testing.T) {
	env := setupProfileEnv(t)
	alice := env.insertUser(t, "alice", "Alice", "")
	bob := env.insertUser(t, "bob", "Bob", "")

	resp := env.postFormAs(t, "/alice/follow", bob, url.Values{"return_to": []string{"/alice?tab=followers"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("follow status %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/alice?tab=followers" {
		t.Fatalf("follow redirect = %q", loc)
	}
	var count int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM follows WHERE follower_user_id = $1 AND followee_user_id = $2`,
		bob.ID, alice.ID,
	).Scan(&count); err != nil {
		t.Fatalf("count follow: %v", err)
	}
	if count != 1 {
		t.Fatalf("follow count = %d, want 1", count)
	}
	body := env.getAs(t, "/alice", bob)
	for _, want := range []string{"FOLLOWING=1", "FOLLOWERS=1", "FOLLOWINGCOUNT=0"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in body: %s", want, body)
		}
	}
	followers := env.getAs(t, "/alice?tab=followers", alice)
	if !strings.Contains(followers, "ITEMS=user:bob;") {
		t.Fatalf("followers tab missing bob: %s", followers)
	}

	resp = env.postFormAs(t, "/alice/unfollow", bob, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unfollow status %d, want 303", resp.StatusCode)
	}
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM follows WHERE follower_user_id = $1 AND followee_user_id = $2`,
		bob.ID, alice.ID,
	).Scan(&count); err != nil {
		t.Fatalf("count unfollow: %v", err)
	}
	if count != 0 {
		t.Fatalf("follow count after unfollow = %d, want 0", count)
	}
}

func TestProfile_FollowSelfRejected(t *testing.T) {
	env := setupProfileEnv(t)
	alice := env.insertUser(t, "alice", "Alice", "")

	resp := env.postFormAs(t, "/alice/follow", alice, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("follow self status %d, want 400", resp.StatusCode)
	}
}

func TestProfile_FollowOrgRoutesUpdateCountsAndState(t *testing.T) {
	env := setupProfileEnv(t)
	owner := env.insertUser(t, "owner", "Owner", "")
	bob := env.insertUser(t, "bob", "Bob", "")
	env.insertOrg(t, "acme", "Acme", "", owner)

	resp := env.postFormAs(t, "/acme/follow", bob, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("follow org status %d, want 303", resp.StatusCode)
	}
	var count int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM follows f JOIN orgs o ON o.id = f.followee_org_id
		 WHERE f.follower_user_id = $1 AND o.slug = 'acme'`,
		bob.ID,
	).Scan(&count); err != nil {
		t.Fatalf("count org follow: %v", err)
	}
	if count != 1 {
		t.Fatalf("org follow count = %d, want 1", count)
	}
	body := env.getAs(t, "/acme", bob)
	for _, want := range []string{"ORG=acme", "FOLLOWING=1", "FOLLOWERS=1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in org body: %s", want, body)
		}
	}
}

func TestProfile_OverviewDataUsesVisibleReposAndOrganizations(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	alice := env.insertUser(t, "alice", "Alice Anderson", "Hi.")
	env.insertOrg(t, "acme", "Acme", "", alice)
	env.insertUserRepo(t, alice.ID, "public-repo", "visible", "public", "Go", 0, 0)
	env.insertUserRepo(t, alice.ID, "private-repo", "hidden", "private", "Rust", 0, 0)

	body := env.getAs(t, "/alice", usersdb.User{})
	for _, want := range []string{
		"VISIBLE=1",
		"ORGS=1",
		"README=false",
		"CONTRIB=0",
		"WEEKS=53",
		"YEARS=4",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body: %s", want, body)
		}
	}
	if strings.Contains(body, "private-repo") {
		t.Fatalf("anonymous profile overview leaked private repo data: %s", body)
	}
}

func TestProfile_ContributionsCountVerifiedAndAffiliatedImportedIdentities(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithRepoFS(t)
	alice := env.insertUser(t, "alice", "Alice Anderson", "Hi.")
	env.insertVerifiedEmail(t, alice.ID, "alice@outlook.com")
	env.insertUserRepo(t, alice.ID, "owned", "user repo", "public", "Go", 0, 0)
	orgID := env.insertOrg(t, "acme", "Acme", "", alice)
	env.insertOrgRepo(t, orgID, "team", "org repo with imported author email", "public", "Rust", 0, 0)
	env.insertOrgRepo(t, orgID, "other", "different author", "public", "Rust", 0, 0)
	bob := env.insertUser(t, "bob", "Bob", "")
	env.insertUserRepo(t, bob.ID, "spoof", "public repo with same author name", "public", "Go", 0, 0)

	now := time.Now().UTC()
	env.writeInitialCommit(t, "alice", "owned", "Alice Anderson", "alice@outlook.com", now.AddDate(0, 0, -7))
	env.writeInitialCommit(t, "acme", "team", "alice", "alice@unverified.example", now.AddDate(0, 0, -14))
	env.writeInitialCommit(t, "acme", "other", "Bob", "bob@example.com", now.AddDate(0, 0, -5))
	env.writeInitialCommit(t, "bob", "spoof", "alice", "alice@spoof.example", now.AddDate(0, 0, -3))

	body := env.getAs(t, "/alice", usersdb.User{})
	for _, want := range []string{
		"CONTRIB=2",
		"PERIOD=in the last year",
		"WEEKS=53",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body: %s", want, body)
		}
	}
}

func TestProfile_ContributionsSelectedYearHasStableLinks(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithRepoFS(t)
	alice := env.insertUser(t, "alice", "Alice Anderson", "")
	env.insertVerifiedEmail(t, alice.ID, "alice@example.com")
	env.insertUserRepo(t, alice.ID, "archive", "old work", "public", "Go", 0, 0)

	currentYear := time.Now().UTC().Year()
	selectedYear := currentYear - 1
	env.writeInitialCommit(t, "alice", "archive", "Alice Anderson", "alice@example.com",
		time.Date(selectedYear, time.March, 10, 12, 0, 0, 0, time.UTC))

	body := env.getAs(t, fmt.Sprintf("/alice?year=%d", selectedYear), usersdb.User{})
	for _, want := range []string{
		"CONTRIB=1",
		"PERIOD=in " + strconv.Itoa(selectedYear),
		fmt.Sprintf("%d:false:/alice;", currentYear),
		fmt.Sprintf("%d:true:/alice?year=%d;", selectedYear, selectedYear),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body: %s", want, body)
		}
	}
}

func TestProfile_PrivateContributionsRequireOwnerOptIn(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithRepoFS(t)
	alice := env.insertUser(t, "alice", "Alice Anderson", "")
	env.insertVerifiedEmail(t, alice.ID, "alice@example.com")
	env.insertUserRepo(t, alice.ID, "public-work", "visible work", "public", "Go", 0, 0)
	env.insertUserRepo(t, alice.ID, "private-work", "private work", "private", "Go", 0, 0)

	now := time.Now().UTC()
	env.writeInitialCommit(t, "alice", "public-work", "Alice Anderson", "alice@example.com", now.AddDate(0, 0, -2))
	env.writeInitialCommit(t, "alice", "private-work", "Alice Anderson", "alice@example.com", now.AddDate(0, 0, -1))

	body := env.getAs(t, "/alice", alice)
	for _, want := range []string{
		"CONTRIB=1",
		"PRIVATE=false",
		"CUSTOMIZE=1 ACTION=/alice/contribution-settings RETURN=/alice",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body: %s", want, body)
		}
	}
	if strings.Contains(body, "private-work") {
		t.Fatalf("self profile leaked private repo name through contribution settings: %s", body)
	}

	currentYear := time.Now().UTC().Year()
	returnTo := fmt.Sprintf("/alice?year=%d", currentYear)
	resp := env.postContributionSettings(t, "/alice/contribution-settings", alice, true, returnTo)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != returnTo {
		t.Fatalf("Location = %q, want %q", loc, returnTo)
	}

	body = env.getAs(t, "/alice", usersdb.User{})
	for _, want := range []string{
		"CONTRIB=2",
		"PRIVATE=true",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body: %s", want, body)
		}
	}
	if strings.Contains(body, "private-work") {
		t.Fatalf("anonymous profile leaked private repo name after private contribution opt-in: %s", body)
	}
}

func TestProfile_ContributionSettingsRequireProfileOwner(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	alice := env.insertUser(t, "alice", "Alice", "")
	bob := env.insertUser(t, "bob", "Bob", "")

	resp := env.postContributionSettings(t, "/alice/contribution-settings", bob, true, "/alice")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}

	body := env.getAs(t, "/alice", alice)
	if !strings.Contains(body, "PRIVATE=false") {
		t.Fatalf("unexpected settings change by non-owner: %s", body)
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

func TestProfile_DispatchesOrgOverviewWithVisibleAggregates(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	creator := env.insertUser(t, "alice", "Alice", "")
	orgID := env.insertOrg(t, "tenseleyflow", "tenseleyFlow", "workflows", creator)
	env.insertOrgRepo(t, orgID, "shithub", "GitHub clone", "public", "Go", 3, 1, "git", "forge")
	env.insertOrgRepo(t, orgID, "private-roadmap", "hidden", "private", "Rust", 2, 0, "secret")

	resp, err := newNonRedirClient(t).Get(env.srv.URL + "/tenseleyflow")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	for _, want := range []string{
		"ORG=tenseleyflow",
		"REPOS=1",
		"PINS=1",
		"MEMBERS=1",
		"PEOPLE=1",
		"NAMES=shithub;",
		"LANGS=Go=1;",
		"TOPICS=forge=1;git=1;",
		"VIEWAS=Public",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in body: %s", want, got)
		}
	}
	if strings.Contains(got, "private-roadmap") || strings.Contains(got, "Rust") {
		t.Fatalf("anonymous org overview leaked private repo data: %s", got)
	}
}

func TestProfile_OrgRepositoriesPagePaginatesVisibleRepos(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	creator := env.insertUser(t, "alice", "Alice", "")
	orgID := env.insertOrg(t, "tenseleyflow", "tenseleyFlow", "workflows", creator)
	for i := 0; i < 31; i++ {
		env.insertOrgRepo(t, orgID, fmt.Sprintf("repo-%02d", i), "public repo", "public", "Go", int64(i), 0)
	}
	env.insertOrgRepo(t, orgID, "private-roadmap", "hidden", "private", "Rust", 99, 0)

	body := env.getAs(t, "/orgs/tenseleyflow/repositories?sort=name&page=2", usersdb.User{})
	for _, want := range []string{
		"ORGREPOS=tenseleyflow",
		"ACTIVE=repositories",
		"TOTAL=31",
		"FILTERED=31",
		"PAGE=2/2",
		"SORT=name",
		"NAMES=repo-30;",
		"P1=false",
		"P2=true",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body: %s", want, body)
		}
	}
	if strings.Contains(body, "private-roadmap") || strings.Contains(body, "Rust") {
		t.Fatalf("anonymous org repositories page leaked private repo data: %s", body)
	}
}

func TestProfile_OrgRepositoriesPageFilters(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	creator := env.insertUser(t, "alice", "Alice", "")
	orgID := env.insertOrg(t, "tenseleyflow", "tenseleyFlow", "workflows", creator)
	env.insertOrgRepo(t, orgID, "shithub", "GitHub clone", "public", "Go", 3, 1, "forge")
	env.insertOrgRepo(t, orgID, "loader", "local agent loop", "public", "Python", 9, 0, "agents")
	env.insertOrgRepo(t, orgID, "sway", "adapter research", "public", "Python", 1, 0, "llm")

	body := env.getAs(t, "/orgs/tenseleyflow/repositories?q=agent&type=public&language=Python&sort=stars", usersdb.User{})
	for _, want := range []string{
		"FILTERED=1",
		"TYPE=public",
		"LANG=Python",
		"SORT=stars",
		"NAMES=loader;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body: %s", want, body)
		}
	}
	if strings.Contains(body, "shithub") || strings.Contains(body, "sway;") {
		t.Fatalf("org repositories filters returned unexpected repo: %s", body)
	}
}

func TestProfile_UserPinsCanBeCustomized(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	alice := env.insertUser(t, "alice", "Alice", "")
	loaderID := env.insertUserRepo(t, alice.ID, "loader", "local assistant", "public", "Python", 0, 0)
	shithubID := env.insertUserRepo(t, alice.ID, "shithub", "GitHub clone", "public", "Go", 3, 1)
	env.insertUserRepo(t, alice.ID, "private-roadmap", "hidden", "private", "Rust", 9, 0)

	got := env.getAs(t, "/alice", alice)
	for _, want := range []string{"SELF=1", "PINS=0", "CANDIDATES=2", "CUSTOMIZE=1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in body: %s", want, got)
		}
	}
	if strings.Contains(got, "private-roadmap") {
		t.Fatalf("private repo was offered as a pin candidate: %s", got)
	}

	resp := env.postPins(t, "/alice/pins", alice, shithubID, loaderID)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/alice#pinned" {
		t.Fatalf("Location = %q", loc)
	}

	got = env.getAs(t, "/alice", usersdb.User{})
	for _, want := range []string{"PINS=2", "PINNAMES=shithub;loader;", "SELECTED=shithub;loader;"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in body: %s", want, got)
		}
	}
	if strings.Contains(got, "CUSTOMIZE=1") {
		t.Fatalf("anonymous viewer saw customize affordance: %s", got)
	}
}

func TestProfile_OrgPinsFallbackUntilCustomized(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	owner := env.insertUser(t, "alice", "Alice", "")
	orgID := env.insertOrg(t, "tenseleyflow", "tenseleyFlow", "workflows", owner)
	env.insertOrgRepo(t, orgID, "shithub", "GitHub clone", "public", "Go", 5, 1)
	loaderID := env.insertOrgRepo(t, orgID, "loader", "local assistant", "public", "Python", 1, 0)
	env.insertOrgRepo(t, orgID, "private-roadmap", "hidden", "private", "Rust", 9, 0)

	got := env.getAs(t, "/tenseleyflow", usersdb.User{})
	for _, want := range []string{"PINS=2", "PINNAMES=shithub;loader;", "CANDIDATES=2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in body: %s", want, got)
		}
	}

	resp := env.postPins(t, "/tenseleyflow/pins", owner, loaderID)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/tenseleyflow#pinned" {
		t.Fatalf("Location = %q", loc)
	}

	got = env.getAs(t, "/tenseleyflow", usersdb.User{})
	for _, want := range []string{"PINS=1", "PINNAMES=loader;", "SELECTED=loader;"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in body: %s", want, got)
		}
	}
	if strings.Contains(got, "PINNAMES=shithub;") || strings.Contains(got, "PINNAMES=loader;shithub;") {
		t.Fatalf("custom org pins fell back to the synthetic set: %s", got)
	}
}

func TestProfile_PinUpdatesRequireOwnershipAndPublicRepos(t *testing.T) {
	t.Parallel()
	env := setupProfileEnv(t)
	owner := env.insertUser(t, "alice", "Alice", "")
	outsider := env.insertUser(t, "bob", "Bob", "")
	orgID := env.insertOrg(t, "tenseleyflow", "tenseleyFlow", "workflows", owner)
	env.insertOrgRepo(t, orgID, "public-repo", "visible", "public", "Go", 0, 0)
	privateID := env.insertOrgRepo(t, orgID, "private-roadmap", "hidden", "private", "Rust", 0, 0)

	resp := env.postPins(t, "/tenseleyflow/pins", outsider)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("outsider status %d, want 403", resp.StatusCode)
	}

	resp = env.postPins(t, "/tenseleyflow/pins", owner, privateID)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("private repo status %d, want 400", resp.StatusCode)
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

func TestProfile_AvatarStreamsOrgAvatar(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	env := setupProfileEnvWithStore(t, store)
	owner := env.insertUser(t, "alice", "Alice", "")
	orgID := env.insertOrg(t, "acme", "Acme", "", owner)
	key := "avatars/orgs/acme/test.png"
	if _, err := store.Put(context.Background(), key, strings.NewReader("org-avatar"), storage.PutOpts{
		ContentType:   "image/png",
		ContentLength: int64(len("org-avatar")),
	}); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE orgs SET avatar_object_key = $1 WHERE id = $2`,
		key, orgID,
	); err != nil {
		t.Fatalf("set org avatar: %v", err)
	}

	resp, _ := http.Get(env.srv.URL + "/avatars/acme")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("content-type %q", resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "org-avatar" {
		t.Fatalf("body=%q", body)
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
