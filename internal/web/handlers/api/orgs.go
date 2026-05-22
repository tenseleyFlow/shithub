// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountOrgs registers the S50 §7 organization REST surface.
//
//	GET /api/v1/user/orgs                  authenticated user's memberships
//	GET /api/v1/users/{username}/orgs      another user's memberships
//	GET /api/v1/orgs/{org}                 single-org fetch
//	GET /api/v1/orgs/{org}/members         org members
//
// All endpoints are scope-gated by user:read (the org membership graph
// is part of the user's profile). Suspended / soft-deleted orgs and
// users 404. Org-membership visibility: shithub has no public/private
// membership distinction in v1, so /users/{u}/orgs returns everything.
func (h *Handlers) mountOrgs(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/user/orgs", h.userOrgsList)
		r.Get("/api/v1/users/{username}/orgs", h.userOrgsListPublic)
		r.Get("/api/v1/orgs/{org}", h.orgGet)
		r.Get("/api/v1/orgs/{org}/members", h.orgMembersList)
	})
}

// ─── presentation ───────────────────────────────────────────────────

type orgResponse struct {
	ID                    int64  `json:"id"`
	Slug                  string `json:"slug"`
	Login                 string `json:"login"` // gh-compatible alias for slug
	DisplayName           string `json:"display_name"`
	Description           string `json:"description,omitempty"`
	Location              string `json:"location,omitempty"`
	Website               string `json:"website,omitempty"`
	Plan                  string `json:"plan"`
	AllowMemberRepoCreate bool   `json:"allow_member_repo_create"`
	CreatedAt             string `json:"created_at"`
}

func presentOrg(o orgsdb.Org) orgResponse {
	return orgResponse{
		ID:                    o.ID,
		Slug:                  o.Slug,
		Login:                 o.Slug,
		DisplayName:           o.DisplayName,
		Description:           o.Description,
		Location:              o.Location,
		Website:               o.Website,
		Plan:                  string(o.Plan),
		AllowMemberRepoCreate: o.AllowMemberRepoCreate,
		CreatedAt:             o.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
}

// orgMembershipResponse is the row shape returned by /user/orgs.
// F47: the CLI's `org list --json` exporter projects gh-canonical
// detail fields (description, location, website, created_at, avatar
// URL) for each row. Pre-fix the listing carried only slug + role,
// and the exporter emitted zero values for everything else. The
// fields below come from the same JOIN as the listing query.
type orgMembershipResponse struct {
	OrgID       int64  `json:"org_id"`
	Slug        string `json:"slug"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	Location    string `json:"location,omitempty"`
	Website     string `json:"website,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	Role        string `json:"role"`
}

type orgMemberResponse struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	JoinedAt    string `json:"joined_at,omitempty"`
}

// ─── /user/orgs ─────────────────────────────────────────────────────

func (h *Handlers) userOrgsList(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	rows, err := orgsdb.New().ListOrgsForUser(r.Context(), h.d.Pool, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list user orgs", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, presentMembershipList(rows))
}

func (h *Handlers) userOrgsListPublic(w http.ResponseWriter, r *http.Request) {
	user, err := usersdb.New().GetUserByUsername(r.Context(), h.d.Pool, chi.URLParam(r, "username"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "user not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: lookup user", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	rows, err := orgsdb.New().ListOrgsForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list user orgs (public)", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, presentMembershipList(rows))
}

func presentMembershipList(rows []orgsdb.ListOrgsForUserRow) []orgMembershipResponse {
	out := make([]orgMembershipResponse, 0, len(rows))
	for _, row := range rows {
		entry := orgMembershipResponse{
			OrgID:       row.OrgID,
			Slug:        row.Slug,
			Login:       row.Slug,
			DisplayName: row.DisplayName,
			Description: row.Description,
			Location:    row.Location,
			Website:     row.Website,
			Role:        string(row.Role),
		}
		if row.AvatarObjectKey.Valid {
			entry.AvatarURL = row.AvatarObjectKey.String
		}
		if row.CreatedAt.Valid {
			entry.CreatedAt = row.CreatedAt.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	return out
}

// ─── /orgs/{org} ────────────────────────────────────────────────────

func (h *Handlers) orgGet(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, presentOrg(org))
}

func (h *Handlers) orgMembersList(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	rows, err := orgsdb.New().ListOrgMembers(r.Context(), h.d.Pool, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list org members", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]orgMemberResponse, 0, len(rows))
	for _, row := range rows {
		entry := orgMemberResponse{
			UserID:      row.UserID,
			Username:    row.Username,
			DisplayName: row.DisplayName,
			Role:        string(row.Role),
		}
		if row.JoinedAt.Valid {
			entry.JoinedAt = row.JoinedAt.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// resolveAPIOrg loads {org} by slug, 404-ing soft-deleted /
// suspended / unknown orgs. Suspended orgs are returned for read
// (matches the web UI's "suspended banner" treatment) but soft-deleted
// ones disappear.
func (h *Handlers) resolveAPIOrg(w http.ResponseWriter, r *http.Request, slug string) (orgsdb.Org, bool) {
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
