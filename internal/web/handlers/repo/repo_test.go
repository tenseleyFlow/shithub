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
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	repoinsights "github.com/tenseleyFlow/shithub/internal/repos/insights"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	repotraffic "github.com/tenseleyFlow/shithub/internal/repos/traffic"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
	"github.com/tenseleyFlow/shithub/internal/worker"
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
	return newRepoFixtureWithEnforce(t, config.EnforceConfig{})
}

// newRepoFixtureWithEnforce mirrors newRepoFixture but plumbs an
// operator-configured per-feature enforce matrix into the handler
// Deps. PRO07 tests pass a config with the relevant user-kind flag
// flipped to true to exercise the enforce path.
func newRepoFixtureWithEnforce(t *testing.T, enforce config.EnforceConfig) *repoFixture {
	return newRepoFixtureWithRenderFS(t, enforce, minimalTemplatesFS(), render.Options{})
}

func newRepoFixtureWithTemplates(t *testing.T, templates fs.FS, opts render.Options) *repoFixture {
	return newRepoFixtureWithRenderFS(t, config.EnforceConfig{}, templates, opts)
}

func newRepoFixtureWithRenderFS(t *testing.T, enforce config.EnforceConfig, templates fs.FS, opts render.Options) *repoFixture {
	t.Helper()
	pool := dbtest.NewTestDB(t)

	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	rr, err := render.New(templates, opts)
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	objectStore := storage.NewMemoryStore()

	h, err := New(Deps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render:         rr,
		Pool:           pool,
		RepoFS:         rfs,
		ObjectStore:    objectStore,
		Audit:          audit.NewRecorder(),
		Limiter:        throttle.NewLimiter(),
		BillingEnforce: enforce,
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
		"_layout.html":                       {Data: layout},
		"_repo_settings_nav.html":            {Data: []byte(`{{ define "repo-settings-nav" }}NAV{{ end }}`)},
		"errors/403.html":                    {Data: body},
		"errors/404.html":                    {Data: body},
		"errors/429.html":                    {Data: body},
		"errors/500.html":                    {Data: body},
		"repo/new.html":                      {Data: []byte(`{{ define "page" }}OWNERS={{ range .Owners }}{{ .Token }}:{{ if eq .Token $.Form.Owner }}selected{{ end }}:{{ .Slug }};{{ end }}{{ end }}`)},
		"repo/actions.html":                  {Data: []byte(`{{ define "page" }}COUNT={{ .RunCount }};FILTERED={{ .FilteredRunCount }};PAGE={{ .Pagination.ResultText }};LISTBASE={{ .ListBasePath }};FILTERQ={{ .FilterQuery }};CLEAR={{ .HasClearableFilters }}:{{ .ClearFiltersHref }};{{ range .FilterMenus }}MENU={{ .Key }}:{{ .Label }}:{{ len .Options }};{{ end }}{{ range .ActionsSidebar.Management }}MGMTNAV={{ .Key }}:{{ .Active }}:{{ .Href }};{{ end }}{{ range .DispatchWorkflows }}DISPATCH={{ .Name }}:{{ .DispatchHref }}:{{ range .Inputs }}{{ .Name }}/{{ .Type }}/{{ .Required }}/{{ .Default }}/{{ range .Options }}{{ .Value }}|{{ end }},{{ end }};{{ end }}{{ range .Workflows }}WF={{ .Name }}:{{ .Count }}:{{ .Active }}:{{ .Href }};PIN={{ .File }}:{{ .Pinned }}:{{ .CanMoveUp }}:{{ .CanMoveDown }};{{ end }}{{ range .Runs }}RUN={{ .Title }}:#{{ .RunIndex }}:{{ .Event }}:{{ .HeadRef }}:{{ .ActorUsername }}:{{ .StateClass }};RUNACTIONS={{ .CanRerun }}:{{ .CanCancel }}:{{ .WorkflowHref }}:{{ .WorkflowFileHref }};{{ end }}{{ end }}`)},
		"repo/actions_new_workflow.html":     {Data: []byte(`{{ define "page" }}NEW={{ .NewWorkflowHref }};{{ with .Error }}ERROR={{ . }};{{ end }}{{ range .SupportedTemplates }}TEMPLATE={{ .Key }}:{{ .Name }}:{{ .Filename }}:{{ .Href }};{{ end }}{{ range .UnsupportedTemplates }}UNSUPPORTED={{ .Key }}:{{ .Name }}:{{ .Reason }};{{ end }}{{ end }}`)},
		"repo/actions_management.html":       {Data: []byte(`{{ define "page" }}MGMT={{ .Page.Key }}:{{ .Page.Title }}:{{ .Page.EmptyTitle }}:{{ .Page.CountLabel }};COUNT={{ .RunCount }};{{ range .Page.Stats }}STAT={{ .Label }}:{{ .Value }};{{ end }}{{ range .Page.Caches }}CACHE={{ .Key }}:{{ .Version }}:{{ .Ref }}:{{ .Size }};{{ end }}{{ range .Page.Runners }}RUNNER={{ .Name }}:{{ .Status }}:{{ .Capacity }}:{{ .ActiveJobCount }}:{{ range .Labels }}{{ . }}|{{ end }};{{ end }}{{ range .Page.UsageRows }}USAGE={{ .WorkflowName }}:{{ .WorkflowFile }}:{{ .RunCount }}:{{ .JobCount }}:{{ .Minutes }};{{ end }}{{ range .Page.PerformanceRows }}PERF={{ .WorkflowName }}:{{ .WorkflowFile }}:{{ .RunCount }}:{{ .JobCount }}:{{ .AvgRuntime }}:{{ .AvgQueue }}:{{ .FailureRate }};{{ end }}{{ range .ActionsSidebar.Management }}MGMTNAV={{ .Key }}:{{ .Active }}:{{ .Href }};{{ end }}{{ range .ActionsSidebar.Workflows }}WF={{ .Name }}:{{ .Active }};PIN={{ .File }}:{{ .Pinned }};{{ end }}{{ end }}`)},
		"repo/editor.html":                   {Data: []byte(`{{ define "page" }}EDITOR={{ .FormAction }}:{{ .PathValue }}:{{ .Ref }};{{ with .Error }}ERROR={{ . }};{{ end }}{{ range .WorkflowDiagnostics }}DIAG={{ .SeverityLabel }}:{{ .Path }}:{{ .Message }};{{ end }}{{ end }}`)},
		"repo/_action_run_status.html":       {Data: []byte(`{{ define "action-run-status" }}STATUS={{ .Run.StateClass }}:{{ .Run.IsTerminal }}:{{ .Run.StatusHref }};{{ end }}`)},
		"repo/settings_branches.html":        {Data: []byte(`{{ define "page" }}GOV={{ .Governance.Scope }}:{{ .Governance.State }}:{{ .Governance.CanUseRequiredReviewers }}:{{ .Governance.CanUseAdvancedBranchProtection }}:{{ .Governance.BillingHref }};RULES={{ len .Rules }};{{ range .Governance.Features }}FEATURE={{ .Name }}:{{ .State }}:{{ .Gated }};{{ end }}{{ end }}`)},
		"repo/action_run.html":               {Data: []byte(`{{ define "page" }}RUN={{ .Run.Title }}:#{{ .Run.RunIndex }}:{{ .Run.Event }}:{{ .Run.ActorUsername }}:{{ .Run.StateClass }};{{ if .Run.ParentRunHref }}PARENT={{ .Run.ParentRunIndex }}:{{ .Run.ParentRunHref }};{{ end }}{{ if .Run.CanRerun }}RERUN={{ .Run.RerunHref }};{{ end }}{{ if .Run.CanCancel }}CANCEL_RUN={{ .Run.CancelHref }};{{ end }}{{ if .Run.CanApprove }}APPROVE={{ .Run.ApproveHref }};REJECT={{ .Run.RejectHref }};{{ end }}{{ if .Run.ApprovalPending }}APPROVAL_PENDING={{ .Run.ApprovalReason }};{{ end }}SUMMARY={{ .Run.JobCount }}:{{ .Run.CompletedCount }}:{{ .Run.FailureCount }}:{{ .Run.ArtifactCount }};ANNOTATIONS={{ .Run.AnnotationCount }}:{{ .Run.WarningCount }}:{{ .Run.ErrorCount }};{{ range .Run.AnnotationGroups }}AGROUP={{ .JobName }}:{{ .Count }};{{ range .Annotations }}ANN={{ .Level }}:{{ .Title }}:{{ .Message }}:{{ .Location }}:{{ .SourceHref }}:{{ .StepHref }};{{ end }}{{ end }}GRAPH={{ .Run.Graph.CanvasWidth }}x{{ .Run.Graph.CanvasHeight }}:{{ len .Run.Graph.Nodes }}:{{ len .Run.Graph.Edges }};{{ range .Run.Graph.Nodes }}GNODE={{ .JobKey }}:{{ .X }}:{{ .Y }}:{{ .StepCount }}:{{ .FailureCount }};{{ end }}{{ range .Run.Jobs }}JOB={{ .Name }}:{{ .StateClass }}:{{ .NeedsText }}:{{ .RunsOn }};{{ if .WaitReason }}WAIT={{ .WaitReason }};{{ end }}{{ if .CanCancel }}CANCEL_JOB={{ .CancelHref }};{{ end }}{{ if .CancelRequested }}CANCEL_REQUESTED={{ .Name }};{{ end }}{{ range .Steps }}STEP={{ .Name }}:{{ .StateClass }}:{{ .LogHref }};{{ end }}{{ end }}{{ end }}`)},
		"repo/action_run_status.html":        {Data: []byte(`{{ define "page" }}{{ template "action-run-status" . }}{{ end }}`)},
		"repo/action_step_log.html":          {Data: []byte(`{{ define "page" }}STEPLOG={{ .Log.Job.Name }}:{{ .Log.Step.Name }}:{{ .Log.LogSource }}:{{ .Log.DownloadHref }}:{{ .Log.LogTruncated }};{{ range .Log.Annotations }}ANN={{ .Level }}:{{ .Title }}:{{ .Message }}:{{ .Location }}:{{ .SourceHref }};{{ end }}{{ with .Log.StreamHref }}STREAM={{ . }};{{ end }}{{ with .Log.LogError }}ERROR={{ . }};{{ end }}LOG={{ .Log.LogText }};{{ end }}`)},
		"repo/settings_actions.html":         {Data: []byte(`{{ define "page" }}POLICY={{ .Policy.ActionsEnabled }}:{{ .Policy.RequirePRApproval }}:{{ .Policy.EffectiveActionsEnabled }}:{{ .Policy.EffectiveRequirePRApproval }}:{{ .Policy.EffectiveMaxRepoQueuedRuns }};{{ with .Error }}ERROR={{ . }}{{ end }}{{ end }}`)},
		"repo/settings_environments.html":    {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }};{{ end }}{{ range .Environments }}ENV={{ .Name }}:{{ .DeploymentBranchPolicy }}:{{ .WaitTimerMinutes }}:{{ .RequiredReviewers }}:{{ .PreventSelfReview }}:{{ .SecretCount }};{{ range .BranchPatterns }}PATTERN={{ . }};{{ end }}{{ end }}{{ with .Selected }}SELECTED={{ .Name }}:{{ .DeploymentBranchPolicy }}:{{ .WaitTimerMinutes }}:{{ .RequiredReviewers }}:{{ .PreventSelfReview }};{{ range .Secrets }}SECRET={{ .Name }};{{ end }}{{ end }}{{ end }}`)},
		"repo/settings_secrets.html":         {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}{{ range .Secrets }}SECRET={{ .Name }};{{ end }}{{ range .Variables }}VAR={{ .Name }}:{{ .Value }};{{ end }}{{ end }}`)},
		"repo/security_code_scanning.html":   {Data: []byte(`{{ define "page" }}UPLOAD={{ .UploadAllowed }};SUMMARY={{ .Summary.OpenAlertCount }}/{{ .Summary.TotalAlertCount }};{{ with .Error }}ERROR={{ . }};{{ end }}{{ with .Success }}SUCCESS={{ . }};{{ end }}{{ range .Alerts }}ALERT={{ .RuleID }}:{{ .Path }}:{{ .Severity }}:{{ .Status }};{{ end }}{{ range .Campaigns }}CAMPAIGN={{ .Title }}:{{ .State }};{{ end }}{{ end }}`)},
		"repo/security_advisories.html":      {Data: []byte(`{{ define "page" }}ADVISORIES={{ len .Advisories }};CAN={{ .CanManageAdvisories }};ALLOWED={{ .WriteGate.Allowed }};COUNTS={{ index .StateCounts "draft" }}/{{ index .StateCounts "published" }}/{{ index .StateCounts "withdrawn" }}/{{ index .StateCounts "archived" }};{{ with .Error }}ERROR={{ . }};{{ end }}{{ range .Advisories }}ADV={{ .Row.Identifier }}:{{ .Row.State }};{{ end }}{{ end }}`)},
		"repo/security_advisory_form.html":   {Data: []byte(`{{ define "page" }}MODE={{ .Mode }};ALLOWED={{ .WriteGate.Allowed }};{{ with .Error }}ERROR={{ . }};{{ end }}{{ with .Advisory }}ADV={{ .Row.Identifier }};{{ end }}{{ end }}`)},
		"repo/security_advisory_detail.html": {Data: []byte(`{{ define "page" }}ADV={{ .Advisory.Row.Identifier }}:{{ .Advisory.Row.State }};DESC={{ .Advisory.RenderedDescription }};{{ range .Events }}EVENT={{ .EventType }}:{{ .OldState }}>{{ .NewState }};{{ end }}{{ end }}`)},
		"repo/pulls_list.html":               {Data: []byte(`{{ define "page" }}{{ range .Items }}PR={{ .Row.Number }}:{{ if .Checks.Show }}{{ .Checks.StateClass }}:{{ .Checks.Label }}:{{ .Checks.Href }}{{ end }};{{ end }}{{ end }}`)},
		"repo/pull_view.html":                {Data: []byte(`{{ define "page" }}STATE={{ .PullStats.CheckState }};SUMMARY={{ if .PullStats.CheckSummary.Show }}{{ .PullStats.CheckSummary.StateClass }}:{{ .PullStats.CheckSummary.Label }}:{{ .PullStats.CheckSummary.Href }}{{ end }};REQ={{ .RequiredChecks.HasRequired }}:{{ .RequiredChecks.Satisfied }}:{{ range .RequiredChecks.Missing }}{{ . }}|{{ end }};DEP={{ if .DependencyReview.Show }}{{ .DependencyReview.StateClass }}:{{ .DependencyReview.Title }}:{{ len .DependencyReview.Items }}:{{ range .DependencyReview.Items }}{{ .Row.PackageName }}:{{ .VersionDelta }}:{{ .SeverityClass }}:{{ .Row.AdvisorySummary }}|{{ end }}{{ end }};{{ range .CheckGroups }}{{ range .Runs }}RUN={{ .AppSlug }}:{{ .R.Name }}:{{ .StateClass }}:{{ .DetailsHref }}:{{ .CanRerun }}:{{ .RerunHref }};{{ end }}{{ end }}{{ end }}`)},
		"repo/commits.html":                  {Data: []byte(`{{ define "page" }}COMMITS={{ .Repo.Name }}:{{ .Ref }}:{{ .Page }};{{ range .CommitGroups }}{{ range .Rows }}{{ if .Checks.Show }}CHECK={{ .Commit.ShortOID }}:{{ .Checks.StateClass }}:{{ .Checks.Href }};{{ end }}{{ end }}{{ end }}{{ end }}`)},
		"repo/commit.html":                   {Data: []byte(`{{ define "page" }}COMMIT={{ .Detail.ShortOID }};{{ if .Checks.Show }}CHECK={{ .Checks.StateClass }}:{{ .Checks.Href }};{{ end }}{{ end }}`)},
		"repo/branches.html":                 {Data: []byte(`{{ define "page" }}{{ range .Rows }}BRANCH={{ .Name }}:{{ if .Checks.Show }}{{ .Checks.StateClass }}:{{ .Checks.Href }}{{ end }};{{ end }}{{ end }}`)},
		"repo/compare.html":                  {Data: []byte(`{{ define "page" }}{{ range .CommitRows }}COMPARE={{ .Commit.ShortOID }}:{{ if .Checks.Show }}{{ .Checks.StateClass }}:{{ .Checks.Href }}{{ end }};{{ end }}{{ end }}`)},
		"repo/insights.html":                 {Data: []byte(`{{ define "page" }}INSIGHTS={{ .Insights.Active }}:queued={{ .Insights.Queued }}:stale={{ .Insights.Stale }}:needs={{ .Insights.NeedsSnapshot }}:views={{ .Insights.Traffic.TotalViews }}{{ end }}`)},
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

func TestRepoInsights_MissingSnapshotEnqueuesRefresh(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	req := repoRouteRequest(http.MethodGet, "/alice/public-repo/pulse", f.owner.Username, f.publicRepo.Name, anonymousViewer())
	rw := httptest.NewRecorder()

	f.handlers.repoInsightsPulse(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rw.Code, rw.Body.String())
	}
	if got, want := rw.Body.String(), "INSIGHTS=pulse:queued=true:stale=false:needs=true"; !strings.Contains(got, want) {
		t.Fatalf("body missing %q: %s", want, got)
	}
	if got := f.countQueuedJobs(t, f.publicRepo.ID); got != 1 {
		t.Fatalf("queued insights jobs = %d, want 1", got)
	}
}

func TestRepoInsights_StaleSnapshotEnqueuesRefresh(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	ctx := context.Background()
	rq := reposdb.New()
	snapshot := repoinsights.EmptySnapshot("trunk", time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC))
	snapshot.HeadSHA = "old-sha"
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if _, err := rq.UpsertRepoInsightSnapshot(ctx, f.pool, reposdb.UpsertRepoInsightSnapshotParams{
		RepoID:           f.publicRepo.ID,
		DefaultBranch:    "trunk",
		HeadSha:          snapshot.HeadSHA,
		CommitCount:      int32(snapshot.CommitCount),
		ContributorCount: int32(snapshot.ContributorCount),
		Additions:        snapshot.Additions,
		Deletions:        snapshot.Deletions,
		Data:             data,
	}); err != nil {
		t.Fatalf("upsert snapshot: %v", err)
	}
	if err := rq.UpdateRepoDefaultBranchOID(ctx, f.pool, reposdb.UpdateRepoDefaultBranchOIDParams{
		ID:               f.publicRepo.ID,
		DefaultBranchOid: pgtype.Text{String: "new-sha", Valid: true},
	}); err != nil {
		t.Fatalf("update default branch oid: %v", err)
	}
	req := repoRouteRequest(http.MethodGet, "/alice/public-repo/graphs/contributors", f.owner.Username, f.publicRepo.Name, anonymousViewer())
	rw := httptest.NewRecorder()

	f.handlers.repoInsightsContributors(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rw.Code, rw.Body.String())
	}
	if got, want := rw.Body.String(), "INSIGHTS=contributors:queued=false:stale=true:needs=true"; !strings.Contains(got, want) {
		t.Fatalf("body missing %q: %s", want, got)
	}
	if got := f.countQueuedJobs(t, f.publicRepo.ID); got != 1 {
		t.Fatalf("queued insights jobs = %d, want 1", got)
	}
}

func TestRepoInsights_PrivateOrgRepoRequiresTeamBilling(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	ctx := context.Background()
	orgID := f.insertOwnedOrg(t, "tenseleyflow")
	privateOrgRepo, err := reposdb.New().CreateRepo(ctx, f.pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: orgID, Valid: true},
		Name:          "private-org-repo",
		Description:   "",
		Visibility:    reposdb.RepoVisibilityPrivate,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo private org: %v", err)
	}
	req := repoRouteRequest(http.MethodGet, "/tenseleyflow/private-org-repo/pulse", "tenseleyflow", privateOrgRepo.Name, viewerFor(f.owner))
	rw := httptest.NewRecorder()

	f.handlers.repoInsightsPulse(rw, req)

	if rw.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d, want 402: %s", rw.Code, rw.Body.String())
	}
	if got, want := rw.Body.String(), "Repository insights require Team billing"; !strings.Contains(got, want) {
		t.Fatalf("body missing %q: %s", want, got)
	}
	if got := f.countQueuedJobs(t, privateOrgRepo.ID); got != 0 {
		t.Fatalf("queued insights jobs = %d, want 0", got)
	}
}

func TestRepoInsights_TrafficLoadsAggregatesWithoutSnapshotJob(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	ctx := context.Background()
	when := time.Now().UTC()
	if err := repotraffic.RecordView(ctx, f.pool, repotraffic.Event{
		RepoID:       f.publicRepo.ID,
		OccurredAt:   when,
		VisitorKey:   "user:1",
		Path:         "/",
		ReferrerHost: "github.com",
	}); err != nil {
		t.Fatalf("RecordView: %v", err)
	}
	req := repoRouteRequest(http.MethodGet, "/alice/public-repo/graphs/traffic", f.owner.Username, f.publicRepo.Name, anonymousViewer())
	rw := httptest.NewRecorder()

	f.handlers.repoInsightsTraffic(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rw.Code, rw.Body.String())
	}
	if got, want := rw.Body.String(), "INSIGHTS=traffic:queued=false:stale=false:needs=false:views=1"; !strings.Contains(got, want) {
		t.Fatalf("body missing %q: %s", want, got)
	}
	if got := f.countQueuedJobs(t, f.publicRepo.ID); got != 0 {
		t.Fatalf("queued insights jobs = %d, want 0", got)
	}
}

func repoRouteRequest(method, target, owner, repoName string, viewer middleware.CurrentUser) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("owner", owner)
	rctx.URLParams.Add("repo", repoName)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	return withViewer(req, viewer)
}

func (f *repoFixture) countQueuedJobs(t *testing.T, repoID int64) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs
		 WHERE kind = $1
		   AND payload->>'repo_id' = $2
		   AND completed_at IS NULL
		   AND failed_at IS NULL`,
		string(worker.KindRepoInsightsRecalc), strconv.FormatInt(repoID, 10)).Scan(&count); err != nil {
		t.Fatalf("count queued insights jobs: %v", err)
	}
	return count
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
