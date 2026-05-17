// SPDX-License-Identifier: AGPL-3.0-or-later

package repos_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/issues"
	"github.com/tenseleyFlow/shithub/internal/repos"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

func TestCollaborationSurfacesProjectAndWiki(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, deps, uid, uname, _ := setupCreateEnv(t)
	res, err := repos.Create(ctx, deps, repos.Params{
		OwnerUserID:   uid,
		OwnerUsername: uname,
		Name:          "collab",
		Visibility:    "public",
	})
	if err != nil {
		t.Fatalf("Create repo: %v", err)
	}

	project, err := repos.CreateProject(ctx, repos.Deps{Pool: deps.Pool}, repos.ProjectInput{
		RepoID:      res.Repo.ID,
		Title:       "Launch board",
		Description: "Track launch work",
		ActorUserID: uid,
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if project.Title != "Launch board" || project.State != reposdb.RepoProjectStateOpen {
		t.Fatalf("project = %+v", project)
	}
	project, err = repos.SetProjectState(ctx, repos.Deps{Pool: deps.Pool}, res.Repo.ID, project.ID, "closed")
	if err != nil {
		t.Fatalf("SetProjectState: %v", err)
	}
	if project.State != reposdb.RepoProjectStateClosed || !project.ClosedAt.Valid {
		t.Fatalf("closed project = %+v", project)
	}
	issue, err := issues.Create(ctx, issues.Deps{Pool: deps.Pool}, issues.CreateParams{
		RepoID: res.Repo.ID, AuthorUserID: uid, Title: "Project item", Body: "",
	})
	if err != nil {
		t.Fatalf("Create issue: %v", err)
	}
	if _, err := repos.AddIssueToProject(ctx, repos.Deps{Pool: deps.Pool}, project.ID, issue.ID, uid); err != nil {
		t.Fatalf("AddIssueToProject: %v", err)
	}
	items, err := reposdb.New().ListRepoProjectItems(ctx, deps.Pool, project.ID)
	if err != nil {
		t.Fatalf("ListRepoProjectItems: %v", err)
	}
	if len(items) != 1 || items[0].IssueNumber != issue.Number || items[0].IssueTitle != issue.Title {
		t.Fatalf("project items = %+v, want issue #%d", items, issue.Number)
	}

	page, err := repos.CreateWikiPage(ctx, repos.Deps{Pool: deps.Pool}, repos.WikiPageInput{
		RepoID:      res.Repo.ID,
		Title:       "Home",
		Slug:        "Home Page",
		Body:        `before <script>alert("xss")</script> after **bold**`,
		ActorUserID: uid,
	})
	if err != nil {
		t.Fatalf("CreateWikiPage: %v", err)
	}
	if page.Slug != "home-page" {
		t.Fatalf("slug=%q, want home-page", page.Slug)
	}
	html := page.BodyHtmlCached.String
	if strings.Contains(strings.ToLower(html), "<script") || !strings.Contains(html, "<strong>") {
		t.Fatalf("wiki html not sanitized/rendered as expected: %q", html)
	}
	_, err = repos.CreateWikiPage(ctx, repos.Deps{Pool: deps.Pool}, repos.WikiPageInput{
		RepoID: res.Repo.ID,
		Title:  "Duplicate",
		Slug:   "home-page",
		Body:   "duplicate",
	})
	if !errors.Is(err, repos.ErrWikiExists) {
		t.Fatalf("duplicate wiki err=%v, want ErrWikiExists", err)
	}
}
