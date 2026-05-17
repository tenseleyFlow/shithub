// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/repos"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type repoWikiPageView struct {
	Page reposdb.RepoWikiPage
	HTML template.HTML
}

type repoWikiFormState struct {
	Mode  string
	Title string
	Slug  string
	Body  string
	Error string
	Page  reposdb.RepoWikiPage
}

func (h *Handlers) repoTabWiki(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	pages, selected, ok := h.loadWikiPages(w, r, row, "")
	if !ok {
		return
	}
	h.renderWiki(w, r, row, owner.Username, pages, selected)
}

func (h *Handlers) repoWikiView(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	pages, selected, ok := h.loadWikiPages(w, r, row, chi.URLParam(r, "slug"))
	if !ok {
		return
	}
	h.renderWiki(w, r, row, owner.Username, pages, selected)
}

func (h *Handlers) loadWikiPages(w http.ResponseWriter, r *http.Request, row reposdb.Repo, slug string) ([]reposdb.RepoWikiPage, *repoWikiPageView, bool) {
	pages, err := h.rq.ListRepoWikiPages(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo wiki: list pages", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return nil, nil, false
	}
	if len(pages) == 0 {
		if slug != "" {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return nil, nil, false
		}
		return pages, nil, true
	}
	var selected reposdb.RepoWikiPage
	if slug != "" {
		selected, err = h.rq.GetRepoWikiPageBySlug(r.Context(), h.d.Pool, reposdb.GetRepoWikiPageBySlugParams{
			RepoID: row.ID,
			Slug:   slug,
		})
	} else {
		selected = pages[0]
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Logger.WarnContext(r.Context(), "repo wiki: get page", "repo_id", row.ID, "slug", slug, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return nil, nil, false
	}
	view := repoWikiPageView{
		Page: selected,
		HTML: template.HTML(selected.BodyHtmlCached.String), //nolint:gosec // sanitized by internal/markdown before storage.
	}
	return pages, &view, true
}

func (h *Handlers) renderWiki(w http.ResponseWriter, r *http.Request, row reposdb.Repo, owner string, pages []reposdb.RepoWikiPage, selected *repoWikiPageView) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	canWrite := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, viewer.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(row)).Allow
	banner, featureAllowed := h.repoFeatureBanner(r.Context(), row, owner, entitlements.FeatureRepoWikis, "Repository wikis")
	canWrite = canWrite && featureAllowed

	data := h.repoHeaderData(r, row, owner, "wiki")
	data["Title"] = "Wiki · " + row.Name
	data["Pages"] = pages
	data["SelectedPage"] = selected
	data["CanWriteWiki"] = canWrite
	data["WikiGateBanner"] = banner
	if err := h.d.Render.RenderPage(w, r, "repo/wiki", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo wiki render", "error", err)
	}
}

func (h *Handlers) repoWikiNew(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureRepoWikis, "Repository wikis") {
		return
	}
	h.renderWikiForm(w, r, row, owner.Username, repoWikiFormState{Mode: "new", Slug: "home"})
}

func (h *Handlers) repoWikiCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureRepoWikis, "Repository wikis") {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	page, err := repos.CreateWikiPage(r.Context(), repos.Deps{Pool: h.d.Pool}, repos.WikiPageInput{
		RepoID:      row.ID,
		Title:       r.PostFormValue("title"),
		Slug:        r.PostFormValue("slug"),
		Body:        r.PostFormValue("body"),
		ActorUserID: viewer.ID,
	})
	if err != nil {
		h.renderWikiFormError(w, r, row, owner.Username, repoWikiFormState{
			Mode:  "new",
			Title: r.PostFormValue("title"),
			Slug:  r.PostFormValue("slug"),
			Body:  r.PostFormValue("body"),
		}, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/wiki/"+page.Slug, http.StatusSeeOther)
}

func (h *Handlers) repoWikiEdit(w http.ResponseWriter, r *http.Request) {
	row, owner, page, ok := h.loadWikiMutation(w, r)
	if !ok {
		return
	}
	h.renderWikiForm(w, r, row, owner, repoWikiFormState{
		Mode:  "edit",
		Title: page.Title,
		Slug:  page.Slug,
		Body:  page.Body,
		Page:  page,
	})
}

func (h *Handlers) repoWikiUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, page, ok := h.loadWikiMutation(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	updated, err := repos.UpdateWikiPage(r.Context(), repos.Deps{Pool: h.d.Pool}, page.ID, repos.WikiPageInput{
		RepoID:      row.ID,
		Title:       r.PostFormValue("title"),
		Slug:        page.Slug,
		Body:        r.PostFormValue("body"),
		ActorUserID: viewer.ID,
	})
	if err != nil {
		h.renderWikiFormError(w, r, row, owner, repoWikiFormState{
			Mode:  "edit",
			Title: r.PostFormValue("title"),
			Slug:  page.Slug,
			Body:  r.PostFormValue("body"),
			Page:  page,
		}, err)
		return
	}
	http.Redirect(w, r, "/"+owner+"/"+row.Name+"/wiki/"+updated.Slug, http.StatusSeeOther)
}

func (h *Handlers) repoWikiDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, page, ok := h.loadWikiMutation(w, r)
	if !ok {
		return
	}
	if err := h.rq.DeleteRepoWikiPage(r.Context(), h.d.Pool, reposdb.DeleteRepoWikiPageParams{ID: page.ID, RepoID: row.ID}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo wiki: delete", "repo_id", row.ID, "page_id", page.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/"+owner+"/"+row.Name+"/wiki", http.StatusSeeOther)
}

func (h *Handlers) loadWikiMutation(w http.ResponseWriter, r *http.Request) (reposdb.Repo, string, reposdb.RepoWikiPage, bool) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return reposdb.Repo{}, "", reposdb.RepoWikiPage{}, false
	}
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureRepoWikis, "Repository wikis") {
		return reposdb.Repo{}, "", reposdb.RepoWikiPage{}, false
	}
	page, err := h.rq.GetRepoWikiPageBySlug(r.Context(), h.d.Pool, reposdb.GetRepoWikiPageBySlugParams{
		RepoID: row.ID,
		Slug:   chi.URLParam(r, "slug"),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Logger.WarnContext(r.Context(), "repo wiki: load mutation page", "repo_id", row.ID, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return reposdb.Repo{}, "", reposdb.RepoWikiPage{}, false
	}
	return row, owner.Username, page, true
}

func (h *Handlers) renderWikiFormError(w http.ResponseWriter, r *http.Request, row reposdb.Repo, owner string, form repoWikiFormState, err error) {
	switch {
	case errors.Is(err, repos.ErrWikiInvalid):
		form.Error = "Title, slug, or body is invalid."
	case errors.Is(err, repos.ErrWikiExists):
		form.Error = "A wiki page with that slug already exists."
	default:
		h.d.Logger.WarnContext(r.Context(), "repo wiki: write", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderWikiForm(w, r, row, owner, form)
}

func (h *Handlers) renderWikiForm(w http.ResponseWriter, r *http.Request, row reposdb.Repo, owner string, form repoWikiFormState) {
	pages, _ := h.rq.ListRepoWikiPages(r.Context(), h.d.Pool, row.ID)
	data := h.repoHeaderData(r, row, owner, "wiki")
	data["Title"] = "Wiki · " + row.Name
	data["Pages"] = pages
	data["Form"] = form
	if err := h.d.Render.RenderPage(w, r, "repo/wiki_form", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo wiki form render", "error", err)
	}
}
