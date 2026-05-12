// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/repos/lifecycle"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountRepos registers the S50 §2 REST surface for repositories.
//
//	GET    /api/v1/user/repos                 list authenticated user's repos
//	GET    /api/v1/users/{username}/repos     list a user's public repos
//	GET    /api/v1/orgs/{org}/repos           list an org's repos (visibility-aware)
//	GET    /api/v1/repos/{owner}/{repo}       fetch a single repo
//	POST   /api/v1/user/repos                 create personal repo
//	POST   /api/v1/orgs/{org}/repos           create org-owned repo
//	PATCH  /api/v1/repos/{owner}/{repo}       update mutable repo settings
//	DELETE /api/v1/repos/{owner}/{repo}       soft-delete a repo
//
// Scopes: repo:read for GETs, repo:write for POST/PATCH/DELETE. Existence
// leaks are smothered behind policy gates that 404 instead of 403 when
// the caller can't see the resource.
func (h *Handlers) mountRepos(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/user/repos", h.userReposList)
		r.Get("/api/v1/users/{username}/repos", h.userPublicReposList)
		r.Get("/api/v1/orgs/{org}/repos", h.orgReposList)
		r.Get("/api/v1/repos/{owner}/{repo}", h.repoGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/user/repos", h.userRepoCreate)
		r.Post("/api/v1/orgs/{org}/repos", h.orgRepoCreate)
		r.Patch("/api/v1/repos/{owner}/{repo}", h.repoPatch)
		r.Delete("/api/v1/repos/{owner}/{repo}", h.repoDelete)
	})
}

// repoResponse mirrors GitHub's repo shape. Field selection is the
// minimum the CLI's `gh repo view` / clone logic needs to operate.
type repoResponse struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	OwnerLogin    string `json:"owner_login"`
	OwnerType     string `json:"owner_type"` // "user" | "org"
	Description   string `json:"description"`
	Visibility    string `json:"visibility"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	Fork          bool   `json:"fork"`
	Archived      bool   `json:"archived"`
	HasIssues     bool   `json:"has_issues"`
	HasPulls      bool   `json:"has_pulls"`
	StarCount     int64  `json:"star_count"`
	WatcherCount  int64  `json:"watcher_count"`
	ForkCount     int64  `json:"fork_count"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

func presentRepo(r reposdb.Repo, ownerLogin string) repoResponse {
	ownerType := "user"
	if r.OwnerOrgID.Valid {
		ownerType = "org"
	}
	return repoResponse{
		ID:            r.ID,
		Name:          r.Name,
		FullName:      ownerLogin + "/" + r.Name,
		OwnerLogin:    ownerLogin,
		OwnerType:     ownerType,
		Description:   r.Description,
		Visibility:    string(r.Visibility),
		Private:       r.Visibility != reposdb.RepoVisibilityPublic,
		DefaultBranch: r.DefaultBranch,
		Fork:          r.ForkOfRepoID.Valid,
		Archived:      r.IsArchived,
		HasIssues:     r.HasIssues,
		HasPulls:      r.HasPulls,
		StarCount:     r.StarCount,
		WatcherCount:  r.WatcherCount,
		ForkCount:     r.ForkCount,
		CreatedAt:     r.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:     r.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
}

// ─── list endpoints ─────────────────────────────────────────────────

func (h *Handlers) userReposList(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := reposdb.New()
	total, err := q.CountReposForOwnerUser(r.Context(), h.d.Pool, pgtype.Int8{Int64: auth.UserID, Valid: true})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count user repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListReposForOwnerUserPaged(r.Context(), h.d.Pool, reposdb.ListReposForOwnerUserPagedParams{
		OwnerUserID: pgtype.Int8{Int64: auth.UserID, Valid: true},
		Limit:       int32(perPage),
		Offset:      int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list user repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	h.writeRepoListPage(w, r, page, perPage, int(total), rows, auth.Username)
}

func (h *Handlers) userPublicReposList(w http.ResponseWriter, r *http.Request) {
	owner, ok := h.resolveAPIUserOwner(w, r, chi.URLParam(r, "username"))
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	q := reposdb.New()
	// Self-view of /users/{me}/repos shows everything (private included),
	// matching GitHub's behavior.
	if auth.UserID == owner.ID {
		page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
		total, err := q.CountReposForOwnerUser(r.Context(), h.d.Pool, pgtype.Int8{Int64: owner.ID, Valid: true})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: count user repos", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		rows, err := q.ListReposForOwnerUserPaged(r.Context(), h.d.Pool, reposdb.ListReposForOwnerUserPagedParams{
			OwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
			Limit:       int32(perPage),
			Offset:      int32((page - 1) * perPage),
		})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: list user repos", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		h.writeRepoListPage(w, r, page, perPage, int(total), rows, owner.Username)
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	total, err := q.CountPublicReposForOwnerUser(r.Context(), h.d.Pool, pgtype.Int8{Int64: owner.ID, Valid: true})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count public repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListPublicReposForOwnerUser(r.Context(), h.d.Pool, reposdb.ListPublicReposForOwnerUserParams{
		OwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
		Limit:       int32(perPage),
		Offset:      int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list public repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	h.writeRepoListPage(w, r, page, perPage, int(total), rows, owner.Username)
}

func (h *Handlers) orgReposList(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrgOwner(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	q := reposdb.New()

	memberView := false
	if auth.UserID != 0 {
		isMem, err := orgs.IsMember(r.Context(), orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, org.ID, auth.UserID)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: org member check", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		memberView = isMem || auth.IsSiteAdmin
	}

	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	if memberView {
		total, err := q.CountReposForOwnerOrg(r.Context(), h.d.Pool, pgtype.Int8{Int64: org.ID, Valid: true})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: count org repos", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		rows, err := q.ListReposForOwnerOrgPaged(r.Context(), h.d.Pool, reposdb.ListReposForOwnerOrgPagedParams{
			OwnerOrgID: pgtype.Int8{Int64: org.ID, Valid: true},
			Limit:      int32(perPage),
			Offset:     int32((page - 1) * perPage),
		})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: list org repos", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		h.writeRepoListPage(w, r, page, perPage, int(total), rows, string(org.Slug))
		return
	}
	total, err := q.CountPublicReposForOwnerOrg(r.Context(), h.d.Pool, pgtype.Int8{Int64: org.ID, Valid: true})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count public org repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListPublicReposForOwnerOrg(r.Context(), h.d.Pool, reposdb.ListPublicReposForOwnerOrgParams{
		OwnerOrgID: pgtype.Int8{Int64: org.ID, Valid: true},
		Limit:      int32(perPage),
		Offset:     int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list public org repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	h.writeRepoListPage(w, r, page, perPage, int(total), rows, string(org.Slug))
}

func (h *Handlers) writeRepoListPage(w http.ResponseWriter, r *http.Request, page, perPage, total int, rows []reposdb.Repo, ownerLogin string) {
	link := apipage.Page{
		Current: page, PerPage: perPage, Total: total,
	}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	out := make([]repoResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentRepo(row, ownerLogin))
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── single-repo GET ────────────────────────────────────────────────

func (h *Handlers) repoGet(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, presentRepo(repo, ownerLogin))
}

// ─── create endpoints ───────────────────────────────────────────────

type repoCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
	Private     *bool  `json:"private,omitempty"`
	AutoInit    bool   `json:"auto_init"`
	License     string `json:"license_template"`
	Gitignore   string `json:"gitignore_template"`
}

// resolvedVisibility picks "public" or "private" from a request, honoring
// either `visibility` (preferred, matches our internal vocab) or the
// gh-compatible `private` boolean. Defaults to "private" — safer than
// public.
func (req repoCreateRequest) resolvedVisibility() (string, error) {
	if req.Visibility != "" {
		switch strings.ToLower(req.Visibility) {
		case "public", "private":
			return strings.ToLower(req.Visibility), nil
		default:
			return "", errors.New("visibility must be public or private")
		}
	}
	if req.Private != nil {
		if *req.Private {
			return "private", nil
		}
		return "public", nil
	}
	return "private", nil
}

func (h *Handlers) userRepoCreate(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var body repoCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	visibility, err := body.resolvedVisibility()
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	params := repos.Params{
		ActorUserID:      auth.UserID,
		ActorIsSiteAdmin: auth.IsSiteAdmin,
		OwnerUserID:      auth.UserID,
		OwnerUsername:    auth.Username,
		Name:             repos.NormalizeName(body.Name),
		Description:      body.Description,
		Visibility:       visibility,
		InitReadme:       body.AutoInit,
		LicenseKey:       body.License,
		GitignoreKey:     body.Gitignore,
	}
	h.runRepoCreate(w, r, params, auth.Username)
}

func (h *Handlers) orgRepoCreate(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	org, ok := h.resolveAPIOrgOwner(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	odeps := orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}
	isMember, err := orgs.IsMember(r.Context(), odeps, org.ID, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: org member check", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "create failed")
		return
	}
	if !isMember && !auth.IsSiteAdmin {
		// Existence-leak parity with the rest of the surface.
		writeAPIError(w, http.StatusNotFound, "org not found")
		return
	}
	isOwner, err := orgs.IsOwner(r.Context(), odeps, org.ID, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: org owner check", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "create failed")
		return
	}
	if !isOwner && !org.AllowMemberRepoCreate && !auth.IsSiteAdmin {
		writeAPIError(w, http.StatusForbidden, "organization restricts repo creation to owners")
		return
	}
	var body repoCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	visibility, err := body.resolvedVisibility()
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	params := repos.Params{
		ActorUserID:      auth.UserID,
		ActorIsSiteAdmin: auth.IsSiteAdmin,
		OwnerOrgID:       org.ID,
		OwnerSlug:        string(org.Slug),
		Name:             repos.NormalizeName(body.Name),
		Description:      body.Description,
		Visibility:       visibility,
		InitReadme:       body.AutoInit,
		LicenseKey:       body.License,
		GitignoreKey:     body.Gitignore,
	}
	h.runRepoCreate(w, r, params, string(org.Slug))
}

func (h *Handlers) runRepoCreate(w http.ResponseWriter, r *http.Request, params repos.Params, ownerLogin string) {
	if h.d.Audit == nil || h.d.Throttle == nil || h.d.RepoFS == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "repo create is not configured")
		return
	}
	res, err := repos.Create(r.Context(), repos.Deps{
		Pool:         h.d.Pool,
		RepoFS:       h.d.RepoFS,
		Audit:        h.d.Audit,
		Limiter:      h.d.Throttle,
		Logger:       h.d.Logger,
		ShithubdPath: h.d.ShithubdPath,
	}, params)
	if err != nil {
		writeRepoCreateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, presentRepo(res.Repo, ownerLogin))
}

func writeRepoCreateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repos.ErrInvalidName),
		errors.Is(err, repos.ErrReservedName),
		errors.Is(err, repos.ErrDescriptionTooLong),
		errors.Is(err, repos.ErrUnknownLicense),
		errors.Is(err, repos.ErrUnknownGitignore):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, repos.ErrTaken):
		writeAPIError(w, http.StatusConflict, "name taken for owner")
	case errors.Is(err, repos.ErrNoVerifiedEmail):
		writeAPIError(w, http.StatusUnprocessableEntity, "actor has no verified primary email")
	default:
		writeAPIError(w, http.StatusInternalServerError, "create failed")
	}
}

// ─── update / delete ────────────────────────────────────────────────

type repoPatchRequest struct {
	Description *string `json:"description,omitempty"`
	HasIssues   *bool   `json:"has_issues,omitempty"`
	HasPulls    *bool   `json:"has_pulls,omitempty"`
	Archived    *bool   `json:"archived,omitempty"`
	Visibility  *string `json:"visibility,omitempty"`
}

func (h *Handlers) repoPatch(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	var body repoPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// General settings (description, has_issues, has_pulls) go through
	// the single UpdateRepoGeneralSettings query so the form-driven HTML
	// surface and this REST path observe the same row updates.
	if body.Description != nil || body.HasIssues != nil || body.HasPulls != nil {
		desc := repo.Description
		if body.Description != nil {
			if err := repos.ValidateDescription(*body.Description); err != nil {
				writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			desc = *body.Description
		}
		hasIssues := repo.HasIssues
		if body.HasIssues != nil {
			hasIssues = *body.HasIssues
		}
		hasPulls := repo.HasPulls
		if body.HasPulls != nil {
			hasPulls = *body.HasPulls
		}
		if err := reposdb.New().UpdateRepoGeneralSettings(r.Context(), h.d.Pool, reposdb.UpdateRepoGeneralSettingsParams{
			ID:          repo.ID,
			Description: desc,
			HasIssues:   hasIssues,
			HasPulls:    hasPulls,
		}); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: repo patch general", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "update failed")
			return
		}
	}
	if body.Archived != nil {
		ldeps := lifecycle.Deps{Pool: h.d.Pool, RepoFS: h.d.RepoFS, Audit: h.d.Audit, Logger: h.d.Logger}
		if *body.Archived && !repo.IsArchived {
			if err := lifecycle.Archive(r.Context(), ldeps, auth.UserID, repo.ID); err != nil {
				h.d.Logger.ErrorContext(r.Context(), "api: archive", "error", err)
				writeAPIError(w, http.StatusInternalServerError, "archive failed")
				return
			}
		} else if !*body.Archived && repo.IsArchived {
			if err := lifecycle.Unarchive(r.Context(), ldeps, auth.UserID, repo.ID); err != nil {
				h.d.Logger.ErrorContext(r.Context(), "api: unarchive", "error", err)
				writeAPIError(w, http.StatusInternalServerError, "unarchive failed")
				return
			}
		}
	}
	if body.Visibility != nil {
		newVis := strings.ToLower(*body.Visibility)
		if newVis != "public" && newVis != "private" {
			writeAPIError(w, http.StatusUnprocessableEntity, "visibility must be public or private")
			return
		}
		if newVis != string(repo.Visibility) {
			ldeps := lifecycle.Deps{Pool: h.d.Pool, RepoFS: h.d.RepoFS, Audit: h.d.Audit, Logger: h.d.Logger}
			if err := lifecycle.SetVisibility(r.Context(), ldeps, auth.UserID, repo.ID, newVis); err != nil {
				h.d.Logger.ErrorContext(r.Context(), "api: set visibility", "error", err)
				writeAPIError(w, http.StatusInternalServerError, "visibility update failed")
				return
			}
		}
	}
	// Re-load the freshest copy so the response reflects all four
	// possible updates in a single payload.
	fresh, err := reposdb.New().GetRepoByID(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: refetch after patch", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	writeJSON(w, http.StatusOK, presentRepo(fresh, ownerLogin))
}

func (h *Handlers) repoDelete(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionRepoDelete)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	ldeps := lifecycle.Deps{Pool: h.d.Pool, RepoFS: h.d.RepoFS, Audit: h.d.Audit, Logger: h.d.Logger}
	if err := lifecycle.SoftDelete(r.Context(), ldeps, auth.UserID, repo.ID); err != nil {
		if errors.Is(err, lifecycle.ErrAlreadyDeleted) {
			writeAPIError(w, http.StatusNotFound, "repo not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: soft delete", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── resolvers ──────────────────────────────────────────────────────

func (h *Handlers) resolveAPIUserOwner(w http.ResponseWriter, r *http.Request, username string) (usersdb.User, bool) {
	user, err := usersdb.New().GetUserByUsername(r.Context(), h.d.Pool, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "user not found")
			return usersdb.User{}, false
		}
		h.d.Logger.ErrorContext(r.Context(), "api: lookup user", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return usersdb.User{}, false
	}
	return user, true
}

func (h *Handlers) resolveAPIOrgOwner(w http.ResponseWriter, r *http.Request, slug string) (orgsdb.Org, bool) {
	org, err := orgsdb.New().GetOrgBySlug(r.Context(), h.d.Pool, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "org not found")
			return orgsdb.Org{}, false
		}
		h.d.Logger.ErrorContext(r.Context(), "api: lookup org", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return orgsdb.Org{}, false
	}
	if org.DeletedAt.Valid {
		writeAPIError(w, http.StatusNotFound, "org not found")
		return orgsdb.Org{}, false
	}
	return org, true
}

// resolveAPIRepoWithLogin loads {owner}/{repo}, runs the policy gate
// (404-on-deny), and additionally returns the owner's login string for
// rendering `full_name`. The login lookup is one extra DB round-trip per
// request — fine for a non-hot path. We compose on top of the existing
// resolveAPIRepo so the existence-leak treatment stays identical.
func (h *Handlers) resolveAPIRepoWithLogin(w http.ResponseWriter, r *http.Request, action policy.Action) (reposdb.Repo, string, bool) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 && actionRequiresAuth(action) {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return reposdb.Repo{}, "", false
	}
	ownerLogin := chi.URLParam(r, "owner")
	repoName := chi.URLParam(r, "repo")
	repo, login, err := lookupRepoByLogin(r, h.d.Pool, ownerLogin, repoName)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "repo not found")
		return reposdb.Repo{}, "", false
	}
	if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), action, policy.NewRepoRefFromRepo(repo)).Allow {
		writeAPIError(w, http.StatusNotFound, "repo not found")
		return reposdb.Repo{}, "", false
	}
	return repo, login, true
}

// actionRequiresAuth returns true for actions that always require a
// logged-in caller (everything except plain read).
func actionRequiresAuth(a policy.Action) bool {
	return a != policy.ActionRepoRead
}

// lookupRepoByLogin tries the user-owner path first, then the org-owner
// path. The login string returned is whichever resolved successfully so
// the caller can plug it into the full_name field.
func lookupRepoByLogin(r *http.Request, pool reposdbPool, ownerLogin, repoName string) (reposdb.Repo, string, error) {
	rq := reposdb.New()
	if user, err := usersdb.New().GetUserByUsername(r.Context(), pool, ownerLogin); err == nil {
		repo, err := rq.GetRepoByOwnerUserAndName(r.Context(), pool, reposdb.GetRepoByOwnerUserAndNameParams{
			OwnerUserID: pgtype.Int8{Int64: user.ID, Valid: true},
			Name:        repoName,
		})
		if err == nil {
			return repo, user.Username, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return reposdb.Repo{}, "", err
		}
	}
	if org, err := orgsdb.New().GetOrgBySlug(r.Context(), pool, ownerLogin); err == nil {
		repo, err := rq.GetRepoByOwnerOrgAndName(r.Context(), pool, reposdb.GetRepoByOwnerOrgAndNameParams{
			OwnerOrgID: pgtype.Int8{Int64: org.ID, Valid: true},
			Name:       repoName,
		})
		if err == nil {
			return repo, string(org.Slug), nil
		}
	}
	return reposdb.Repo{}, "", pgx.ErrNoRows
}

// reposdbPool aliases the pgx DBTX interface that all sqlc-generated
// methods accept; declaring it here keeps this file from importing
// pgxpool directly for what is effectively a typed parameter.
type reposdbPool = reposdb.DBTX
