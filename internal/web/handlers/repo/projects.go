// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type repoProjectView struct {
	Project reposdb.RepoProject
	Items   []reposdb.ListRepoProjectItemsRow
}

type issueProjectOption struct {
	Project  reposdb.RepoProject
	Selected bool
}

func (h *Handlers) repoTabProjects(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	projects, err := h.rq.ListRepoProjects(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo projects: list", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	views := make([]repoProjectView, 0, len(projects))
	for _, project := range projects {
		items, err := h.rq.ListRepoProjectItems(r.Context(), h.d.Pool, project.ID)
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "repo projects: list items", "repo_id", row.ID, "project_id", project.ID, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		views = append(views, repoProjectView{Project: project, Items: items})
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	canManage := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, viewer.PolicyActor(), policy.ActionIssueLabel, policy.NewRepoRefFromRepo(row)).Allow
	banner, featureAllowed := h.repoFeatureBanner(r.Context(), row, owner.Username, entitlements.FeatureRepoProjects, "Repository projects")
	canManage = canManage && featureAllowed

	data := h.repoHeaderData(r, row, owner.Username, "projects")
	data["Title"] = "Projects · " + row.Name
	data["Projects"] = views
	data["CanManageProjects"] = canManage
	data["ProjectGateBanner"] = banner
	if err := h.d.Render.RenderPage(w, r, "repo/projects", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo projects render", "error", err)
	}
}

func (h *Handlers) repoProjectCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureRepoProjects, "Repository projects") {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := repos.CreateProject(r.Context(), repos.Deps{Pool: h.d.Pool}, repos.ProjectInput{
		RepoID:      row.ID,
		Title:       r.PostFormValue("title"),
		Description: r.PostFormValue("description"),
		ActorUserID: viewer.ID,
	}); err != nil {
		h.handleProjectError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/projects", http.StatusSeeOther)
}

func (h *Handlers) repoProjectUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, projectID, ok := h.loadProjectMutation(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := repos.UpdateProject(r.Context(), repos.Deps{Pool: h.d.Pool}, projectID, repos.ProjectInput{
		RepoID:      row.ID,
		Title:       r.PostFormValue("title"),
		Description: r.PostFormValue("description"),
		ActorUserID: viewer.ID,
	}); err != nil {
		h.handleProjectError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner+"/"+row.Name+"/projects", http.StatusSeeOther)
}

func (h *Handlers) repoProjectSetState(w http.ResponseWriter, r *http.Request) {
	row, owner, projectID, ok := h.loadProjectMutation(w, r)
	if !ok {
		return
	}
	if _, err := repos.SetProjectState(r.Context(), repos.Deps{Pool: h.d.Pool}, row.ID, projectID, r.PostFormValue("state")); err != nil {
		h.handleProjectError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner+"/"+row.Name+"/projects", http.StatusSeeOther)
}

func (h *Handlers) repoProjectDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, projectID, ok := h.loadProjectMutation(w, r)
	if !ok {
		return
	}
	if err := h.rq.DeleteRepoProject(r.Context(), h.d.Pool, reposdb.DeleteRepoProjectParams{ID: projectID, RepoID: row.ID}); err != nil {
		h.handleProjectError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner+"/"+row.Name+"/projects", http.StatusSeeOther)
}

func (h *Handlers) loadProjectMutation(w http.ResponseWriter, r *http.Request) (reposdb.Repo, string, int64, bool) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return reposdb.Repo{}, "", 0, false
	}
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureRepoProjects, "Repository projects") {
		return reposdb.Repo{}, "", 0, false
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || projectID <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return reposdb.Repo{}, "", 0, false
	}
	return row, owner.Username, projectID, true
}

func (h *Handlers) issueToggleProject(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureRepoProjects, "Repository projects") {
		return
	}
	issue, ok := h.loadIssueByNumber(w, r, row)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(r.PostFormValue("project_id"), 10, 64)
	if err != nil || projectID <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "project required")
		return
	}
	if _, err := h.rq.GetRepoProject(r.Context(), h.d.Pool, reposdb.GetRepoProjectParams{ID: projectID, RepoID: row.ID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}
	if r.PostFormValue("mode") == "remove" {
		err = h.rq.RemoveIssueFromRepoProject(r.Context(), h.d.Pool, reposdb.RemoveIssueFromRepoProjectParams{ProjectID: projectID, IssueID: issue.ID})
	} else {
		viewer := middleware.CurrentUserFromContext(r.Context())
		_, err = repos.AddIssueToProject(r.Context(), repos.Deps{Pool: h.d.Pool}, projectID, issue.ID, viewer.ID)
	}
	if err != nil {
		h.handleProjectError(w, r, err)
		return
	}
	h.redirectIssueOrPull(w, r, owner.Username, row.Name, issue)
}

func (h *Handlers) issueProjectData(ctx context.Context, row reposdb.Repo, issueID int64) ([]reposdb.RepoProject, []issueProjectOption) {
	assigned, err := h.rq.ListRepoProjectsForIssue(ctx, h.d.Pool, issueID)
	if err != nil {
		return nil, nil
	}
	all, err := h.rq.ListRepoProjects(ctx, h.d.Pool, row.ID)
	if err != nil {
		return assigned, nil
	}
	selected := make(map[int64]bool, len(assigned))
	for _, project := range assigned {
		selected[project.ID] = true
	}
	options := make([]issueProjectOption, 0, len(all))
	for _, project := range all {
		if project.State != reposdb.RepoProjectStateOpen {
			continue
		}
		options = append(options, issueProjectOption{Project: project, Selected: selected[project.ID]})
	}
	return assigned, options
}

func (h *Handlers) canEditIssueProjects(ctx context.Context, row reposdb.Repo, ownerSlug string, actor policy.Actor) bool {
	if !policy.Can(ctx, policy.Deps{Pool: h.d.Pool}, actor, policy.ActionIssueLabel, policy.NewRepoRefFromRepo(row)).Allow {
		return false
	}
	_, allowed := h.repoFeatureBanner(ctx, row, ownerSlug, entitlements.FeatureRepoProjects, "Repository projects")
	return allowed
}

func (h *Handlers) redirectIssueOrPull(w http.ResponseWriter, r *http.Request, owner, repo string, issue issuesdb.Issue) {
	if issue.Kind == issuesdb.IssueKindPr {
		http.Redirect(w, r, "/"+owner+"/"+repo+"/pulls/"+strconv.FormatInt(issue.Number, 10), http.StatusSeeOther)
		return
	}
	h.redirectIssue(w, r, owner, repo, issue.Number)
}

func (h *Handlers) handleProjectError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, repos.ErrProjectInvalid), errors.Is(err, repos.ErrProjectState):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid project")
	default:
		h.d.Logger.WarnContext(r.Context(), "repo projects: write", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}
