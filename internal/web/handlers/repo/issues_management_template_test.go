// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestIssueManagementPagesRenderGitHubLikeLandmarks(t *testing.T) {
	t.Parallel()

	renderer, err := render.New(os.DirFS("../../templates"), render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		t.Fatalf("render.New on real templates: %v", err)
	}

	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	base := map[string]any{
		"CSRFToken":    "test-token",
		"Owner":        "octo",
		"Repo":         reposdb.Repo{ID: 1, Name: "demo", Visibility: reposdb.RepoVisibilityPublic, HasIssues: true},
		"RepoActions":  repoActionView{IsLoggedIn: false, LoginURL: "/login"},
		"RepoCounts":   repoSubnavData{Issues: 12},
		"CanSettings":  true,
		"ActiveSubnav": "issues",
	}

	type issueListTemplateItem struct {
		Issue        issuesdb.Issue
		AuthorName   string
		Labels       []issuesdb.Label
		Assignees    []issuesdb.ListIssueAssigneesRow
		CommentCount int64
	}
	issueData := cloneTemplateData(base)
	issueData["Title"] = "Issues · demo"
	issueData["State"] = "open"
	issueData["OpenCount"] = int64(10)
	issueData["ClosedCount"] = int64(27)
	issueData["Items"] = []issueListTemplateItem{{
		Issue: issuesdb.Issue{
			Number:    47,
			Title:     "execute_and_capture should preserve newlines",
			State:     issuesdb.IssueStateOpen,
			CreatedAt: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		},
		AuthorName: "octo",
		Labels: []issuesdb.Label{{
			ID:          1,
			Name:        "tech-debt",
			Color:       "e6ccb3",
			Description: "Known shortcuts to revisit",
		}},
		CommentCount: 2,
	}}
	assertTemplateContains(t, renderer, "repo/issues_list", "/octo/demo/issues", issueData,
		`Search all issues`,
		`Labels</a>`,
		`Milestones</a>`,
		`New issue</a>`,
		`Author`,
		`Assignees`,
		`Newest`,
		`execute_and_capture should preserve newlines`,
		`tech-debt`,
		`aria-label="2 comments"`,
	)

	labelData := cloneTemplateData(base)
	labelData["Title"] = "Labels · demo"
	labelData["CanManageIssue"] = true
	labelData["Labels"] = []issuesdb.Label{{
		ID:          1,
		Name:        "enhancement",
		Color:       "a2eeef",
		Description: "New feature or request",
	}}
	assertTemplateContains(t, renderer, "repo/labels", "/octo/demo/labels", labelData,
		`Search all labels`,
		`New label`,
		`1 label`,
		`enhancement`,
		`New feature or request`,
		`Sort`,
	)

	milestoneData := cloneTemplateData(base)
	milestoneData["Title"] = "Milestones · demo"
	milestoneData["CanManageIssue"] = true
	milestoneData["State"] = "open"
	milestoneData["OpenCount"] = int64(0)
	milestoneData["ClosedCount"] = int64(0)
	milestoneData["Milestones"] = []issuesdb.Milestone{}
	assertTemplateContains(t, renderer, "repo/milestones", "/octo/demo/milestones", milestoneData,
		`Milestones`,
		`New milestone`,
		`created any Milestones.`,
		`Use Milestones to create collections of Issues and Pull Requests for a particular release or project.`,
		`Create a milestone`,
	)
}

func cloneTemplateData(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func assertTemplateContains(t *testing.T, renderer *render.Renderer, page, path string, data map[string]any, wants ...string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rw := httptest.NewRecorder()
	if err := renderer.RenderPage(rw, req, page, data); err != nil {
		t.Fatalf("RenderPage(%s): %v", page, err)
	}
	body := rw.Body.String()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered %s missing %q in %s", page, want, body)
		}
	}
}
