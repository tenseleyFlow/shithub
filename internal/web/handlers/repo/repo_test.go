// SPDX-License-Identifier: AGPL-3.0-or-later

// Smoke-level integration tests for the highest-traffic repo handler
// helpers. Two-pass authorization (per the S00–S25 audit, finding H7)
// is the kind of subtle logic that handler tests catch and orchestrator
// tests miss — so we cover the visibility/policy invariants directly
// here rather than indirectly through repos.Create or pulls.Merge.
//
// Skip-when-no-DB: dbtest.NewTestDB skips the test if
// SHITHUB_TEST_DATABASE_URL is unset, so unit-test machines without
// Postgres still go green.

package repo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// fixtureHash is a static argon2 PHC test fixture (zero salt, zero key)
// — not a real credential. Same shape as the one used in
// internal/repos/create_test.go.
const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type repoFixture struct {
	pool        *pgxpool.Pool
	handlers    *Handlers
	objectStore storage.ObjectStore
	owner       usersdb.User
	stranger    usersdb.User
	publicRepo  reposdb.Repo
	privateRepo reposdb.Repo
}

// newRepoFixture sets up a Handlers wired to a fresh test DB with two
// users (owner alice, stranger bob) and two repos (public + private).
func newRepoFixture(t *testing.T) *repoFixture {
	t.Helper()
	pool := dbtest.NewTestDB(t)

	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	rr, err := render.New(minimalTemplatesFS(), render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	objectStore := storage.NewMemoryStore()

	h, err := New(Deps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render:      rr,
		Pool:        pool,
		RepoFS:      rfs,
		ObjectStore: objectStore,
		Audit:       audit.NewRecorder(),
		Limiter:     throttle.NewLimiter(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	uq := usersdb.New()
	rq := reposdb.New()
	ctx := context.Background()

	owner, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	stranger, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "bob", DisplayName: "Bob", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	pubRepo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:          "public-repo",
		Description:   "",
		Visibility:    reposdb.RepoVisibilityPublic,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo public: %v", err)
	}
	privRepo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:          "private-repo",
		Description:   "",
		Visibility:    reposdb.RepoVisibilityPrivate,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo private: %v", err)
	}

	return &repoFixture{
		pool:        pool,
		handlers:    h,
		objectStore: objectStore,
		owner:       owner,
		stranger:    stranger,
		publicRepo:  pubRepo,
		privateRepo: privRepo,
	}
}

// minimalTemplatesFS returns just the error pages that
// loadRepoAndAuthorize needs in order to render its 404/403 responses.
func minimalTemplatesFS() fstest.MapFS {
	layout := []byte(`{{ define "layout" }}{{ template "page" . }}{{ end }}`)
	body := []byte(`{{ define "page" }}{{ .StatusText }}: {{ .Message }}{{ end }}`)
	return fstest.MapFS{
		"_layout.html":                 {Data: layout},
		"_repo_settings_nav.html":      {Data: []byte(`{{ define "repo-settings-nav" }}NAV{{ end }}`)},
		"errors/403.html":              {Data: body},
		"errors/404.html":              {Data: body},
		"errors/429.html":              {Data: body},
		"errors/500.html":              {Data: body},
		"repo/new.html":                {Data: []byte(`{{ define "page" }}OWNERS={{ range .Owners }}{{ .Token }}:{{ if eq .Token $.Form.Owner }}selected{{ end }}:{{ .Slug }};{{ end }}{{ end }}`)},
		"repo/actions.html":            {Data: []byte(`{{ define "page" }}COUNT={{ .RunCount }};FILTERED={{ .FilteredRunCount }};PAGE={{ .Pagination.ResultText }};{{ range .DispatchWorkflows }}DISPATCH={{ .Name }}:{{ .DispatchHref }}:{{ range .Inputs }}{{ .Name }}/{{ .Type }}/{{ .Required }}/{{ .Default }}/{{ range .Options }}{{ .Value }}|{{ end }},{{ end }};{{ end }}{{ range .Workflows }}WF={{ .Name }}:{{ .Count }}:{{ .Active }};{{ end }}{{ range .Runs }}RUN={{ .Title }}:#{{ .RunIndex }}:{{ .Event }}:{{ .HeadRef }}:{{ .ActorUsername }}:{{ .StateClass }};{{ end }}{{ end }}`)},
		"repo/_action_run_status.html": {Data: []byte(`{{ define "action-run-status" }}STATUS={{ .Run.StateClass }}:{{ .Run.IsTerminal }}:{{ .Run.StatusHref }};{{ end }}`)},
		"repo/action_run.html":         {Data: []byte(`{{ define "page" }}RUN={{ .Run.Title }}:#{{ .Run.RunIndex }}:{{ .Run.Event }}:{{ .Run.ActorUsername }}:{{ .Run.StateClass }};{{ if .Run.ParentRunHref }}PARENT={{ .Run.ParentRunIndex }}:{{ .Run.ParentRunHref }};{{ end }}{{ if .Run.CanRerun }}RERUN={{ .Run.RerunHref }};{{ end }}{{ if .Run.CanCancel }}CANCEL_RUN={{ .Run.CancelHref }};{{ end }}SUMMARY={{ .Run.JobCount }}:{{ .Run.CompletedCount }}:{{ .Run.FailureCount }}:{{ .Run.ArtifactCount }};{{ range .Run.Jobs }}JOB={{ .Name }}:{{ .StateClass }}:{{ .NeedsText }}:{{ .RunsOn }};{{ if .CanCancel }}CANCEL_JOB={{ .CancelHref }};{{ end }}{{ if .CancelRequested }}CANCEL_REQUESTED={{ .Name }};{{ end }}{{ range .Steps }}STEP={{ .Name }}:{{ .StateClass }}:{{ .LogHref }};{{ end }}{{ end }}{{ end }}`)},
		"repo/action_run_status.html":  {Data: []byte(`{{ define "page" }}{{ template "action-run-status" . }}{{ end }}`)},
		"repo/action_step_log.html":    {Data: []byte(`{{ define "page" }}STEPLOG={{ .Log.Job.Name }}:{{ .Log.Step.Name }}:{{ .Log.LogSource }}:{{ .Log.DownloadURL }}:{{ .Log.LogTruncated }};{{ with .Log.StreamHref }}STREAM={{ . }};{{ end }}{{ with .Log.LogError }}ERROR={{ . }};{{ end }}LOG={{ .Log.LogText }};{{ end }}`)},
		"repo/settings_secrets.html":   {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}{{ range .Secrets }}SECRET={{ .Name }};{{ end }}{{ range .Variables }}VAR={{ .Name }}:{{ .Value }};{{ end }}{{ end }}`)},
	}
}

// withViewer attaches a CurrentUser to the request context the same way
// the OptionalUser middleware would.
func withViewer(req *http.Request, viewer middleware.CurrentUser) *http.Request {
	return req.WithContext(middleware.WithCurrentUserForTest(req.Context(), viewer))
}

func TestSafeLocalPath(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"/alice/repo":              true,
		"/alice/repo/issues/1?x=1": true,
		"":                         false,
		"alice/repo":               false,
		"//evil.example/path":      false,
		"https://evil.example":     false,
	}
	for path, want := range tests {
		if got := safeLocalPath(path); got != want {
			t.Errorf("safeLocalPath(%q) = %v; want %v", path, got, want)
		}
	}
}

func TestNewRepoForm_PreselectsAllowedOrgOwnerHint(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "tenseleyflow")

	req := httptest.NewRequest(http.MethodGet, "/new?owner=tenseleyflow", nil)
	req = withViewer(req, viewerFor(f.owner))
	rw := httptest.NewRecorder()
	f.handlers.newRepoForm(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rw.Code)
	}
	want := "org:" + strconv.FormatInt(orgID, 10) + ":selected:tenseleyflow;"
	if !strings.Contains(rw.Body.String(), want) {
		t.Fatalf("org owner was not preselected; want %q in %s", want, rw.Body.String())
	}
	userSelected := "user:" + strconv.FormatInt(f.owner.ID, 10) + ":selected:alice;"
	if strings.Contains(rw.Body.String(), userSelected) {
		t.Fatalf("personal owner unexpectedly selected: %s", rw.Body.String())
	}
}

// callLoad invokes loadRepoAndAuthorize via a test handler so we can
// exercise the chi URL-param plumbing the way the real router does.
// Returns (status, ok) — `ok` is what loadRepoAndAuthorize returned.
func (f *repoFixture) callLoad(t *testing.T, owner, name string, viewer middleware.CurrentUser, action policy.Action) (int, bool) {
	t.Helper()
	var gotOK bool
	mux := chi.NewRouter()
	mux.Get("/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		_, _, ok := f.handlers.loadRepoAndAuthorize(w, r, action)
		gotOK = ok
		if ok {
			w.WriteHeader(http.StatusOK)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/"+owner+"/"+name, nil)
	req = withViewer(req, viewer)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)
	return rw.Code, gotOK
}

func TestLookupRepoForViewer_PublicRepoVisibleToAnonymous(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	ctx := context.Background()
	row, err := f.handlers.lookupRepoForViewer(ctx, f.owner.Username, f.publicRepo.Name, anonymousViewer())
	if err != nil {
		t.Fatalf("public repo + anon: unexpected err %v", err)
	}
	if row.ID != f.publicRepo.ID {
		t.Errorf("got repo %d; want %d", row.ID, f.publicRepo.ID)
	}
}

func TestLookupRepoForViewer_PrivateRepoHiddenFromAnonymous(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	ctx := context.Background()
	_, err := f.handlers.lookupRepoForViewer(ctx, f.owner.Username, f.privateRepo.Name, anonymousViewer())
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("private repo + anon: want ErrNoRows (privacy-preserving), got %v", err)
	}
}

func TestLookupRepoForViewer_PrivateRepoVisibleToOwner(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	ctx := context.Background()
	row, err := f.handlers.lookupRepoForViewer(ctx, f.owner.Username, f.privateRepo.Name, viewerFor(f.owner))
	if err != nil {
		t.Fatalf("private repo + owner: unexpected err %v", err)
	}
	if row.ID != f.privateRepo.ID {
		t.Errorf("got repo %d; want %d", row.ID, f.privateRepo.ID)
	}
}

func TestLookupRepoForViewer_PrivateRepoHiddenFromStranger(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	ctx := context.Background()
	_, err := f.handlers.lookupRepoForViewer(ctx, f.owner.Username, f.privateRepo.Name, viewerFor(f.stranger))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("private repo + stranger: want ErrNoRows, got %v", err)
	}
}

// loadRepoAndAuthorize on a private repo with an anonymous viewer
// returns 404, NOT 403 — leaking 403 would tell the attacker the repo
// exists. This is the H7 audit invariant.
func TestLoadRepoAndAuthorize_PrivateRepo_Anon_404(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	status, ok := f.callLoad(t, f.owner.Username, f.privateRepo.Name, anonymousViewer(), policy.ActionRepoAdmin)
	if ok {
		t.Fatal("expected ok=false")
	}
	if status != http.StatusNotFound {
		t.Errorf("status: got %d; want 404 (privacy-preserving)", status)
	}
}

// On a public repo, loadRepoAndAuthorize for an admin action returns
// an honest 403 — the viewer can see the repo exists; they just can't
// admin it.
func TestLoadRepoAndAuthorize_PublicRepo_Anon_403(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	status, ok := f.callLoad(t, f.owner.Username, f.publicRepo.Name, anonymousViewer(), policy.ActionRepoAdmin)
	if ok {
		t.Fatal("expected ok=false")
	}
	if status != http.StatusForbidden {
		t.Errorf("status: got %d; want 403 (honest deny)", status)
	}
}

func TestLoadRepoAndAuthorize_OwnerOnPrivate_OK(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	status, ok := f.callLoad(t, f.owner.Username, f.privateRepo.Name, viewerFor(f.owner), policy.ActionRepoAdmin)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if status != http.StatusOK {
		t.Errorf("status: got %d; want 200", status)
	}
}

func anonymousViewer() middleware.CurrentUser {
	return middleware.CurrentUser{}
}

func viewerFor(u usersdb.User) middleware.CurrentUser {
	return middleware.CurrentUser{
		ID:       u.ID,
		Username: u.Username,
	}
}

func (f *repoFixture) insertOwnedOrg(t *testing.T, slug string) int64 {
	t.Helper()
	var orgID int64
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO orgs (slug, display_name, created_by_user_id)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		slug, slug, f.owner.ID).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, f.owner.ID); err != nil {
		t.Fatalf("insert org member: %v", err)
	}
	return orgID
}
