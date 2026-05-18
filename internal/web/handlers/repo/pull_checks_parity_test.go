// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/checks"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestPullListRendersCheckStatusSummary(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	issue, _, headSHA := f.insertPullForChecks(t, f.publicRepo)
	f.createCheckForPull(t, f.publicRepo.ID, headSHA, checkFixture{
		Name:       "ci",
		Status:     "completed",
		Conclusion: "success",
		DetailsURL: "/alice/public-repo/actions/runs/7",
		AppSlug:    "shithub-actions",
	})

	body := f.getPullsBody(t, viewerFor(f.owner), "/alice/public-repo/pulls")
	want := "PR=" + int64String(issue.Number) + ":success:1 check successful:/alice/public-repo/actions/runs/7;"
	if !strings.Contains(body, want) {
		t.Fatalf("missing pull-list check summary %q in %s", want, body)
	}
}

func TestPullViewCheckSummaryRollups(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     string
		conclusion string
		want       string
	}{
		{name: "success", status: "completed", conclusion: "success", want: "SUMMARY=success:1 check successful:/alice/public-repo/actions/runs/12;"},
		{name: "failure", status: "completed", conclusion: "failure", want: "SUMMARY=failure:1 check failed:/alice/public-repo/actions/runs/12;"},
		{name: "pending", status: "in_progress", want: "SUMMARY=pending:1 check pending:/alice/public-repo/actions/runs/12;"},
		{name: "skipped", status: "completed", conclusion: "skipped", want: "SUMMARY=skipped:1 check skipped:/alice/public-repo/actions/runs/12;"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newRepoFixture(t)
			issue, _, headSHA := f.insertPullForChecks(t, f.publicRepo)
			f.createCheckForPull(t, f.publicRepo.ID, headSHA, checkFixture{
				Name:       "ci",
				Status:     tt.status,
				Conclusion: tt.conclusion,
				DetailsURL: "/alice/public-repo/actions/runs/12",
				AppSlug:    "shithub-actions",
			})

			body := f.getPullsBody(t, viewerFor(f.owner), "/alice/public-repo/pulls/"+int64String(issue.Number))
			if !strings.Contains(body, tt.want) {
				t.Fatalf("missing rollup %q in %s", tt.want, body)
			}
		})
	}
}

func TestPullViewRequiredChecksSafeLinksAndRerunControls(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	issue, _, headSHA := f.insertPullForChecks(t, f.publicRepo)
	f.requireChecks(t, f.publicRepo.ID, "trunk", []string{"ci", "lint"})
	f.createCheckForPull(t, f.publicRepo.ID, headSHA, checkFixture{
		Name:       "ci",
		Status:     "completed",
		Conclusion: "success",
		DetailsURL: "/alice/public-repo/actions/runs/21",
		AppSlug:    "shithub-actions",
	})
	f.createCheckForPull(t, f.publicRepo.ID, headSHA, checkFixture{
		Name:       "lint",
		Status:     "completed",
		Conclusion: "failure",
		DetailsURL: "https://evil.example/lint",
		AppSlug:    "external-ci",
	})
	f.createCheckForPull(t, f.publicRepo.ID, headSHA, checkFixture{
		Name:       "report",
		Status:     "completed",
		Conclusion: "success",
		DetailsURL: "/alice/public-repo/actions/runs/22",
		AppSlug:    "external-ci",
	})

	path := "/alice/public-repo/pulls/" + int64String(issue.Number)
	body := f.getPullsBody(t, viewerFor(f.owner), path)
	for _, want := range []string{
		"REQ=true:false:lint|",
		"RUN=shithub-actions:ci:success:/alice/public-repo/actions/runs/21:true:/alice/public-repo/actions/runs/21/rerun;",
		"RUN=external-ci:lint:failure::false:;",
		"RUN=external-ci:report:success:/alice/public-repo/actions/runs/22:false:;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "evil.example") {
		t.Fatalf("external details URL leaked into PR surface: %s", body)
	}

	body = f.getPullsBody(t, viewerFor(f.stranger), path)
	if strings.Contains(body, ":true:/alice/public-repo/actions/runs/21/rerun;") {
		t.Fatalf("rerun control leaked to non-writer: %s", body)
	}
	if !strings.Contains(body, "RUN=shithub-actions:ci:success:/alice/public-repo/actions/runs/21:false:") {
		t.Fatalf("non-writer should still see safe details link without rerun: %s", body)
	}
}

func TestPullChecksPrivateRepoNoLeakForUnauthorizedViewer(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	issue, _, headSHA := f.insertPullForChecks(t, f.privateRepo)
	f.createCheckForPull(t, f.privateRepo.ID, headSHA, checkFixture{
		Name:       "ci",
		Status:     "completed",
		Conclusion: "failure",
		DetailsURL: "/alice/private-repo/actions/runs/99",
		AppSlug:    "shithub-actions",
	})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/private-repo/pulls/"+int64String(issue.Number), nil)
	f.pullsMuxWithViewer(viewerFor(f.stranger)).ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want privacy-preserving 404; body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if strings.Contains(body, "actions/runs/99") || strings.Contains(body, "SUMMARY=") || strings.Contains(body, "RUN=") {
		t.Fatalf("private check state leaked in response body: %s", body)
	}
}

func TestPullProductionTemplateRendersCheckParity(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithTemplates(t, os.DirFS("../../templates"), render.Options{Octicons: render.BuiltinOcticons()})
	issue, _, headSHA := f.insertPullForChecks(t, f.publicRepo)
	f.createCheckForPull(t, f.publicRepo.ID, headSHA, checkFixture{
		Name:       "ci",
		Status:     "completed",
		Conclusion: "success",
		DetailsURL: "/alice/public-repo/actions/runs/31",
		AppSlug:    "shithub-actions",
	})

	body := f.getPullsBody(t, viewerFor(f.owner), "/alice/public-repo/pulls/"+int64String(issue.Number))
	for _, want := range []string{
		"1 check successful",
		"/alice/public-repo/actions/runs/31",
		"/alice/public-repo/actions/runs/31/rerun",
		`name="csrf_token"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("production template missing %q", want)
		}
	}
}

type checkFixture struct {
	Name       string
	Status     string
	Conclusion string
	DetailsURL string
	AppSlug    string
}

func (f *repoFixture) insertPullForChecks(t *testing.T, repo reposdb.Repo) (issuesdb.Issue, pullsdb.PullRequest, string) {
	t.Helper()
	ctx := context.Background()
	issueRow, err := issues.Create(ctx, issues.Deps{Pool: f.pool}, issues.CreateParams{
		RepoID:       repo.ID,
		AuthorUserID: f.owner.ID,
		Title:        "Check parity",
		Body:         "Testing PR checks",
		Kind:         "pr",
	})
	if err != nil {
		t.Fatalf("issues.Create: %v", err)
	}
	headSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pr, err := pullsdb.New().CreatePullRequest(ctx, f.pool, pullsdb.CreatePullRequestParams{
		IssueID:    issueRow.ID,
		BaseRef:    "trunk",
		HeadRef:    "feature-" + int64String(issueRow.Number),
		HeadRepoID: repo.ID,
		BaseOid:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		HeadOid:    headSHA,
		Draft:      false,
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if err := pullsdb.New().SetPullRequestMergeability(ctx, f.pool, pullsdb.SetPullRequestMergeabilityParams{
		IssueID:        issueRow.ID,
		MergeableState: pullsdb.PrMergeableStateClean,
		Mergeable:      pgtype.Bool{Bool: true, Valid: true},
	}); err != nil {
		t.Fatalf("SetPullRequestMergeability: %v", err)
	}
	return issueRow, pr, headSHA
}

func (f *repoFixture) createCheckForPull(t *testing.T, repoID int64, headSHA string, fx checkFixture) {
	t.Helper()
	_, err := checks.Create(context.Background(), checks.Deps{Pool: f.pool}, checks.CreateParams{
		RepoID:     repoID,
		HeadSHA:    headSHA,
		AppSlug:    fx.AppSlug,
		Name:       fx.Name,
		Status:     fx.Status,
		Conclusion: fx.Conclusion,
		DetailsURL: fx.DetailsURL,
	})
	if err != nil {
		t.Fatalf("checks.Create: %v", err)
	}
}

func (f *repoFixture) requireChecks(t *testing.T, repoID int64, branch string, names []string) {
	t.Helper()
	ctx := context.Background()
	q := reposdb.New()
	ruleID, err := q.UpsertBranchProtectionRule(ctx, f.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID:               repoID,
		Pattern:              branch,
		Target:               "branch",
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Int64: f.owner.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertBranchProtectionRule: %v", err)
	}
	if err := q.UpdateBranchProtectionCheckSettings(ctx, f.pool, reposdb.UpdateBranchProtectionCheckSettingsParams{
		ID:                   ruleID,
		StatusChecksRequired: names,
	}); err != nil {
		t.Fatalf("UpdateBranchProtectionCheckSettings: %v", err)
	}
}

func (f *repoFixture) pullsMuxWithViewer(viewer middleware.CurrentUser) http.Handler {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withViewer(r, viewer))
		})
	})
	f.handlers.MountPulls(mux)
	return mux
}

func (f *repoFixture) getPullsBody(t *testing.T, viewer middleware.CurrentUser, path string) string {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	f.pullsMuxWithViewer(viewer).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d, want 200; body=%s", path, resp.Code, resp.Body.String())
	}
	return resp.Body.String()
}

func int64String(v int64) string {
	return strconv.FormatInt(v, 10)
}
