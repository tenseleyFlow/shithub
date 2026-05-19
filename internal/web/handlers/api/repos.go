// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
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

// repoOwnerEnvelope is the nested owner shape gh-compat clients
// (shithub-cli's repos.Repo, gh CLI's Repository) expect. Lives
// alongside the legacy flat OwnerLogin / OwnerType fields during
// the S62 audit migration (one release cycle, same as S60 users).
type repoOwnerEnvelope struct {
	Login string `json:"login"`
	Type  string `json:"type"`
	ID    int64  `json:"id,omitempty"`
}

// repoLicenseEnvelope is the gh-compat license shape. SPDX id is what
// the CLI displays under `repo view`; we fill it from the stored
// license_key column and leave name/url empty when we don't have a
// catalog lookup yet.
type repoLicenseEnvelope struct {
	Key string `json:"key"`
}

// repoResponse mirrors GitHub's repo shape. The S62 audit (B14)
// added the nested owner envelope + html_url + topics + license +
// language + size + pushed_at. Legacy flat fields stay during the
// transition so existing clients keep parsing; new clients should
// consume the envelopes / gh-canonical fields.
type repoResponse struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	// Legacy flat owner fields.
	OwnerLogin string `json:"owner_login"`
	OwnerType  string `json:"owner_type"` // "user" | "org"
	// GitHub-compat nested envelope.
	Owner         *repoOwnerEnvelope   `json:"owner,omitempty"`
	Description   string               `json:"description"`
	Visibility    string               `json:"visibility"`
	Private       bool                 `json:"private"`
	HTMLURL       string               `json:"html_url,omitempty"`
	DefaultBranch string               `json:"default_branch"`
	Fork          bool                 `json:"fork"`
	Archived      bool                 `json:"archived"`
	IsTemplate    bool                 `json:"is_template"`
	HasIssues     bool                 `json:"has_issues"`
	HasPulls      bool                 `json:"has_pulls"`
	StarCount     int64                `json:"star_count"`
	WatcherCount  int64                `json:"watcher_count"`
	ForkCount     int64                `json:"fork_count"`
	Topics        []string             `json:"topics,omitempty"`
	License       *repoLicenseEnvelope `json:"license,omitempty"`
	Language      string               `json:"language,omitempty"`
	// Size is reported in KB to match gh-compat; the DB stores bytes.
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// PushedAt is best-effort: we don't track a separate push time
	// column, so emit updated_at. Active repos see them converge in
	// gh-compat anyway.
	PushedAt string `json:"pushed_at,omitempty"`
}

// presentRepo builds the gh-compat response for one repo. Topics +
// baseURL are passed in so the function stays pure; callers that
// don't have them (e.g. legacy code paths) can supply nil + "" to get
// a response without those fields populated.
func presentRepo(r reposdb.Repo, ownerLogin string, topics []string, baseURL string) repoResponse {
	ownerType := "user"
	if r.OwnerOrgID.Valid {
		ownerType = "org"
	}
	repoRef := policy.NewRepoRefFromRepo(r)
	resp := repoResponse{
		ID:         r.ID,
		Name:       r.Name,
		FullName:   ownerLogin + "/" + r.Name,
		OwnerLogin: ownerLogin,
		OwnerType:  ownerType,
		Owner: &repoOwnerEnvelope{
			Login: ownerLogin,
			Type:  capitalizeFirst(ownerType),
		},
		Description:   r.Description,
		Visibility:    string(r.Visibility),
		Private:       repoRef.IsPrivate(),
		DefaultBranch: r.DefaultBranch,
		Fork:          r.ForkOfRepoID.Valid,
		Archived:      r.IsArchived,
		IsTemplate:    r.IsTemplate,
		HasIssues:     r.HasIssues,
		HasPulls:      r.HasPulls,
		StarCount:     r.StarCount,
		WatcherCount:  r.WatcherCount,
		ForkCount:     r.ForkCount,
		Size:          r.DiskUsedBytes / 1024,
		CreatedAt:     r.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:     r.UpdatedAt.Time.UTC().Format(time.RFC3339),
		PushedAt:      r.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
	if r.OwnerUserID.Valid {
		resp.Owner.ID = r.OwnerUserID.Int64
	} else if r.OwnerOrgID.Valid {
		resp.Owner.ID = r.OwnerOrgID.Int64
	}
	if r.LicenseKey.Valid && r.LicenseKey.String != "" {
		resp.License = &repoLicenseEnvelope{Key: r.LicenseKey.String}
	}
	if r.PrimaryLanguage.Valid {
		resp.Language = r.PrimaryLanguage.String
	}
	if len(topics) > 0 {
		resp.Topics = topics
	}
	if baseURL != "" {
		resp.HTMLURL = strings.TrimRight(baseURL, "/") + "/" + ownerLogin + "/" + r.Name
	}
	return resp
}

// topicsFor fetches the topic set for a repo. Returns nil on lookup
// failure (best-effort: a topic lookup error must not break the
// repo response).
func (h *Handlers) topicsFor(ctx context.Context, repoID int64) []string {
	rows, err := reposdb.New().ListRepoTopics(ctx, h.d.Pool, repoID)
	if err != nil || len(rows) == 0 {
		return nil
	}
	return rows
}

// capitalizeFirst returns s with its first rune upper-cased. Used to
// project the internal lowercase owner_type ("user"|"org") onto the
// GitHub-compat title-case form ("User"|"Organization"-ish — we use
// "User" / "Organization" exactly).
func capitalizeFirst(s string) string {
	switch s {
	case "user":
		return "User"
	case "org":
		return "Organization"
	}
	return s
}

// ─── list endpoints ─────────────────────────────────────────────────

func (h *Handlers) userReposList(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	visibility, verr := strictVisibility(r.URL.Query().Get("visibility"))
	if verr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, verr.Error())
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
	rows = filterReposByVisibility(rows, visibility)
	h.writeRepoListPage(w, r, page, perPage, int(total), rows, auth.Username)
}

// strictVisibility validates the `visibility` query parameter. Empty
// returns ("", nil) — no filter. Anything outside
// {public, private, internal} is 422 (E15: pre-fix would silently
// return all repos for unknown values).
func strictVisibility(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "public", "private", "internal":
		return strings.ToLower(strings.TrimSpace(s)), nil
	default:
		return "", fmt.Errorf("visibility: must be public, private, or internal (got %q)", s)
	}
}

// filterReposByVisibility narrows the row set when a visibility filter
// is present. Empty filter returns rows unchanged.
func filterReposByVisibility(rows []reposdb.Repo, visibility string) []reposdb.Repo {
	if visibility == "" {
		return rows
	}
	filtered := rows[:0]
	for _, row := range rows {
		if strings.EqualFold(string(row.Visibility), visibility) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func (h *Handlers) userPublicReposList(w http.ResponseWriter, r *http.Request) {
	owner, ok := h.resolveAPIUserOwner(w, r, chi.URLParam(r, "username"))
	if !ok {
		return
	}
	visibility, verr := strictVisibility(r.URL.Query().Get("visibility"))
	if verr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, verr.Error())
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
		rows = filterReposByVisibility(rows, visibility)
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
	rows = filterReposByVisibility(rows, visibility)
	h.writeRepoListPage(w, r, page, perPage, int(total), rows, owner.Username)
}

func (h *Handlers) orgReposList(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrgOwner(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	visibility, verr := strictVisibility(r.URL.Query().Get("visibility"))
	if verr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, verr.Error())
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
		rows = filterReposByVisibility(rows, visibility)
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
	rows = filterReposByVisibility(rows, visibility)
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
		// List endpoint: skip topics lookup to avoid N+1. The single
		// GET path populates them; CLI list views don't render them.
		out = append(out, presentRepo(row, ownerLogin, nil, h.d.BaseURL))
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── single-repo GET ────────────────────────────────────────────────

func (h *Handlers) repoGet(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, presentRepo(repo, ownerLogin, h.topicsFor(r.Context(), repo.ID), h.d.BaseURL))
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
	// Brand-new repo — no topics yet, skip the lookup.
	writeJSON(w, http.StatusCreated, presentRepo(res.Repo, ownerLogin, nil, h.d.BaseURL))
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
	case errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded):
		writeAPIError(w, http.StatusPaymentRequired, err.Error())
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
	// Name dispatches into lifecycle.Rename when non-nil. Closes
	// audit finding C7: the previous behavior silently dropped the
	// field, letting `shithub repo rename` print "Renamed to <old
	// name>" against a no-op response.
	Name *string `json:"name,omitempty"`
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
	// C7: dispatch rename through the existing lifecycle.Rename
	// pipeline (validate → tx insert-redirect + UPDATE → FS move →
	// audit). Rename requires repo admin (matches the HTML form
	// gate); the outer ActionRepoSettingsGeneral check already passed
	// for the broader PATCH, so we add the stricter check only when
	// the name field is present.
	if body.Name != nil {
		if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(),
			policy.ActionRepoAdmin, policy.NewRepoRefFromRepo(repo)).Allow {
			writeAPIError(w, http.StatusForbidden, "rename requires repo admin")
			return
		}
		ldeps := lifecycle.Deps{Pool: h.d.Pool, RepoFS: h.d.RepoFS, Audit: h.d.Audit, Logger: h.d.Logger}
		err := lifecycle.Rename(r.Context(), ldeps, lifecycle.RenameParams{
			ActorUserID: auth.UserID,
			RepoID:      repo.ID,
			OwnerUserID: repo.OwnerUserID.Int64,
			OwnerName:   ownerLogin,
			OldName:     repo.Name,
			NewName:     *body.Name,
		})
		if err != nil {
			h.writeRenameError(w, r, err)
			return
		}
		// Reload so the returned repo reflects the new name, and so any
		// sibling fields the caller patched in the same request observe
		// the rename'd row state. Repo struct is captured by-value above
		// (used by the visibility/archived branches); refresh the local
		// `repo` and let the rest of the handler keep working.
		fresh, err := reposdb.New().GetRepoByID(r.Context(), h.d.Pool, repo.ID)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: refetch after rename", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "reload failed")
			return
		}
		repo = fresh
		policy.InvalidateRepo(r.Context(), repo.ID)
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
		wantArchived := *body.Archived
		currentlyArchived := repo.IsArchived
		switch {
		case wantArchived && !currentlyArchived:
			if err := lifecycle.Archive(r.Context(), ldeps, auth.UserID, repo.ID); err != nil {
				h.d.Logger.ErrorContext(r.Context(), "api: archive", "error", err)
				writeAPIError(w, http.StatusInternalServerError, "archive failed")
				return
			}
		case !wantArchived && currentlyArchived:
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
				if errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
					writeAPIError(w, http.StatusPaymentRequired, err.Error())
					return
				}
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
	writeJSON(w, http.StatusOK, presentRepo(fresh, ownerLogin, h.topicsFor(r.Context(), fresh.ID), h.d.BaseURL))
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

// writeRenameError maps lifecycle.Rename errors onto REST status
// codes. Distinct from the HTML lifecycleError mapper because the
// REST surface returns JSON-envelope errors via writeAPIError and
// uses 422 (validation) / 409 (conflict) shapes gh-compat clients
// recognize, where the HTML form uses 400 with plain text.
func (h *Handlers) writeRenameError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, lifecycle.ErrSameName):
		writeAPIError(w, http.StatusUnprocessableEntity, "new name equals current name")
	case errors.Is(err, lifecycle.ErrInvalidName):
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid repository name")
	case errors.Is(err, lifecycle.ErrReservedName):
		writeAPIError(w, http.StatusUnprocessableEntity, "reserved repository name")
	case errors.Is(err, lifecycle.ErrNameTaken):
		writeAPIError(w, http.StatusConflict, "name already taken on this owner")
	case errors.Is(err, lifecycle.ErrRenameRateLimited):
		writeAPIError(w, http.StatusTooManyRequests, "rename rate limit (5 per 30 days) exceeded")
	default:
		h.d.Logger.ErrorContext(r.Context(), "api: rename", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "rename failed")
	}
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
// logged-in caller. Read-shaped actions pass through anonymously so
// the visibility gate inside policy.Can does the talking.
func actionRequiresAuth(a policy.Action) bool {
	switch a {
	case policy.ActionRepoRead, policy.ActionIssueRead, policy.ActionPullRead:
		return false
	default:
		return true
	}
}

// lookupRepoByLogin tries the user-owner path first, then the org-owner
// path. The login string returned is whichever resolved successfully so
// the caller can plug it into the full_name field.
//
// PRO-EXT01-11b: enforces PAT repo binding. If the request is
// authenticated via a token bound to a different repo, the resolution
// returns pgx.ErrNoRows so handlers naturally 404 — preserving the
// "this repo doesn't exist from your perspective" semantic without
// leaking that the binding was the actual reason.
func lookupRepoByLogin(r *http.Request, pool reposdbPool, ownerLogin, repoName string) (reposdb.Repo, string, error) {
	rq := reposdb.New()
	if user, err := usersdb.New().GetUserByUsername(r.Context(), pool, ownerLogin); err == nil {
		repo, err := rq.GetRepoByOwnerUserAndName(r.Context(), pool, reposdb.GetRepoByOwnerUserAndNameParams{
			OwnerUserID: pgtype.Int8{Int64: user.ID, Valid: true},
			Name:        repoName,
		})
		if err == nil {
			if !patBindingAllowsRepo(r, repo.ID) {
				return reposdb.Repo{}, "", pgx.ErrNoRows
			}
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
			if !patBindingAllowsRepo(r, repo.ID) {
				return reposdb.Repo{}, "", pgx.ErrNoRows
			}
			return repo, string(org.Slug), nil
		}
	}
	return reposdb.Repo{}, "", pgx.ErrNoRows
}

// patBindingAllowsRepo reports whether the request's PAT auth (if any)
// permits acting on the given repo. Pure-session requests (no PAT auth)
// always allow.
func patBindingAllowsRepo(r *http.Request, repoID int64) bool {
	auth := middleware.PATAuthFromContext(r.Context())
	return pat.RepoBindingAllows(auth.RepoBinding, repoID)
}

// reposdbPool aliases the pgx DBTX interface that all sqlc-generated
// methods accept; declaring it here keeps this file from importing
// pgxpool directly for what is effectively a typed parameter.
type reposdbPool = reposdb.DBTX
