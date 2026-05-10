// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/social"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountSocial registers the star/watch/stargazers/watchers routes.
// The auth-required group is the caller's responsibility (the
// stargazers / watchers GETs are public, gated only by the visibility
// check inside lookupRepoForViewer).
func (h *Handlers) MountSocial(r chi.Router) {
	r.Get("/{owner}/{repo}/stargazers", h.stargazersList)
	r.Get("/{owner}/{repo}/watchers", h.watchersList)

	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireUser)
		r.Post("/{owner}/{repo}/star", h.repoStar)
		r.Post("/{owner}/{repo}/unstar", h.repoUnstar)
		r.Post("/{owner}/{repo}/watch", h.repoWatch)
	})
}

// socialDeps materializes a social.Deps from the handler-set deps.
// Limiter is shared with the rest of the handler surface so the per-
// user star/unstar cap composes with the existing rate-limit envelope.
func (h *Handlers) socialDeps() social.Deps {
	return social.Deps{
		Pool:    h.d.Pool,
		Limiter: h.d.Limiter,
		Logger:  h.d.Logger,
		Audit:   h.d.Audit,
	}
}

// pageSize is the spec's day-1 lean: 50 rows per page on social
// list pages. Aligns with the issues / PR pagination shape.
const socialPageSize = 50

// repoStar handles POST /{owner}/{repo}/star.
func (h *Handlers) repoStar(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.authorizeSocialAction(w, r, policy.ActionStarCreate)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := social.Star(r.Context(), h.socialDeps(), viewer.ID, row.ID, repoIsPublic(row)); err != nil {
		h.handleSocialError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner+"/"+row.Name, http.StatusSeeOther)
}

// repoUnstar handles POST /{owner}/{repo}/unstar. Same shape as star;
// the orchestrator's idempotency makes it safe even if the user
// already removed the star.
func (h *Handlers) repoUnstar(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.authorizeSocialAction(w, r, policy.ActionStarCreate)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := social.Unstar(r.Context(), h.socialDeps(), viewer.ID, row.ID, repoIsPublic(row)); err != nil {
		h.handleSocialError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner+"/"+row.Name, http.StatusSeeOther)
}

// repoWatch handles POST /{owner}/{repo}/watch with a level form
// field. Level "default" (or empty) deletes the row (returns to the
// implicit `participating` default); explicit "all" / "participating"
// / "ignore" upserts the level.
func (h *Handlers) repoWatch(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.authorizeSocialAction(w, r, policy.ActionWatchSet)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	level := r.PostFormValue("level")
	var err error
	if level == "" || level == "default" {
		err = social.UnsetWatch(r.Context(), h.socialDeps(), viewer.ID, row.ID)
	} else {
		err = social.SetWatch(r.Context(), h.socialDeps(), viewer.ID, row.ID, social.WatchLevel(level))
	}
	if err != nil {
		h.handleSocialError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner+"/"+row.Name, http.StatusSeeOther)
}

// stargazersList renders /{owner}/{repo}/stargazers. Read-public on
// public repos; private repos delegate to lookupRepoForViewer (which
// 404s for non-collab — same shape as the rest of the repo views).
func (h *Handlers) stargazersList(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	name := chi.URLParam(r, "repo")
	row, err := h.lookupRepoForViewer(r.Context(), owner, name, middleware.CurrentUserFromContext(r.Context()))
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	page := pageFromRequest(r)
	q := socialdb.New()
	rows, err := q.ListStargazersForRepo(r.Context(), h.d.Pool, socialdb.ListStargazersForRepoParams{
		RepoID: row.ID, Limit: socialPageSize, Offset: int32((page - 1) * socialPageSize),
	})
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "list stargazers")
		return
	}
	count, _ := q.CountStargazersForRepo(r.Context(), h.d.Pool, row.ID)
	common := map[string]any{
		"Title":       "Stargazers · " + row.Name,
		"Owner":       owner,
		"Repo":        row,
		"Stargazers":  rows,
		"Total":       count,
		"Page":        page,
		"HasNext":     int64(page*socialPageSize) < count,
		"HasPrev":     page > 1,
		"RepoCounts":  h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings": h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
	}
	if err := h.d.Render.RenderPage(w, r, "repo/stargazers", common); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "stargazers render", "error", err)
	}
}

// watchersList renders /{owner}/{repo}/watchers. Same gating as
// stargazers.
func (h *Handlers) watchersList(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	name := chi.URLParam(r, "repo")
	row, err := h.lookupRepoForViewer(r.Context(), owner, name, middleware.CurrentUserFromContext(r.Context()))
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	page := pageFromRequest(r)
	q := socialdb.New()
	rows, err := q.ListWatchersForRepo(r.Context(), h.d.Pool, socialdb.ListWatchersForRepoParams{
		RepoID: row.ID, Limit: socialPageSize, Offset: int32((page - 1) * socialPageSize),
	})
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "list watchers")
		return
	}
	count, _ := q.CountWatchersForRepo(r.Context(), h.d.Pool, row.ID)
	common := map[string]any{
		"Title":       "Watchers · " + row.Name,
		"Owner":       owner,
		"Repo":        row,
		"Watchers":    rows,
		"Total":       count,
		"Page":        page,
		"HasNext":     int64(page*socialPageSize) < count,
		"HasPrev":     page > 1,
		"RepoCounts":  h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings": h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
	}
	if err := h.d.Render.RenderPage(w, r, "repo/watchers", common); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "watchers render", "error", err)
	}
}

// authorizeSocialAction is the social-action equivalent of
// loadRepoAndAuthorize: resolves owner+repo, calls policy.Can with
// the action (which returns DenyVisibility on private+non-collab),
// and routes the deny path through Maybe404. Returns the repo + the
// raw owner string so handlers can build redirects.
func (h *Handlers) authorizeSocialAction(w http.ResponseWriter, r *http.Request, action policy.Action) (repoRow, string, bool) {
	ownerName := chi.URLParam(r, "owner")
	repoName := chi.URLParam(r, "repo")
	row, err := h.lookupRepoForViewer(r.Context(), ownerName, repoName, middleware.CurrentUserFromContext(r.Context()))
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return repoRow{}, "", false
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	repoRef := policy.NewRepoRefFromRepo(row)
	if dec := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, action, repoRef); !dec.Allow {
		h.d.Render.HTTPError(w, r, policy.Maybe404(dec, repoRef, actor), "")
		return repoRow{}, "", false
	}
	return repoRow{ID: row.ID, Name: row.Name, Visibility: string(row.Visibility)}, ownerName, true
}

// repoRow is the slim view the social handlers need from
// `lookupRepoForViewer`. Avoids importing the big reposdb.Repo shape
// into the handler signature.
type repoRow struct {
	ID         int64
	Name       string
	Visibility string
}

func repoIsPublic(r repoRow) bool { return r.Visibility == "public" }

func pageFromRequest(r *http.Request) int {
	v, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if v < 1 {
		return 1
	}
	return v
}

// handleSocialError maps the orchestrator's typed errors to status
// codes / friendly messages. Mirrors the issues/PR error-mapping
// shape so the rest of the handler set stays consistent.
func (h *Handlers) handleSocialError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, social.ErrNotLoggedIn):
		http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
	case errors.Is(err, social.ErrInvalidWatchLevel):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid watch level")
	case errors.Is(err, social.ErrStarRateLimit):
		h.d.Render.HTTPError(w, r, http.StatusTooManyRequests, "rate limit")
	default:
		h.d.Logger.ErrorContext(r.Context(), "social handler", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}
