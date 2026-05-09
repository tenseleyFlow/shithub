// SPDX-License-Identifier: AGPL-3.0-or-later

// Package profile owns the read-only public profile handlers:
// /{username} and /avatars/{username}. Edit-profile is S10.
//
// Route ordering is critical here: the wildcard /{username} catches any
// path the chi router didn't already match. The reserved-name list is the
// second line of defense if a future top-level route is added but not
// registered before the wildcard. The route-audit test in
// internal/web/handlers/handlers_test.go enforces both lines of defense.
package profile

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	"github.com/tenseleyFlow/shithub/internal/avatars"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps wires the profile handlers.
type Deps struct {
	Logger *slog.Logger
	Render *render.Renderer
	Pool   *pgxpool.Pool
	// ObjectStore is used to stream uploaded avatars. May be nil in tests
	// or when S3 is not configured — falls back to identicon.
	ObjectStore storage.ObjectStore
}

// Handlers is the registered profile handler set.
type Handlers struct {
	d Deps
	q *usersdb.Queries
}

// New constructs the handler set, validating Deps.
func New(d Deps) (*Handlers, error) {
	if d.Render == nil {
		return nil, errors.New("profile: nil Render")
	}
	if d.Pool == nil {
		return nil, errors.New("profile: nil Pool")
	}
	return &Handlers{d: d, q: usersdb.New()}, nil
}

// MountAvatars registers /avatars/{username}. Belongs in the CSRF-exempt
// group since GETs are idempotent and benefit from CDN caching.
func (h *Handlers) MountAvatars(r chi.Router) {
	r.Get("/avatars/{username}", h.serveAvatar)
}

// MountProfile registers the /{username} catch-all. Caller MUST pass an
// r that has already mounted every static top-level route — chi matches
// in registration order, and {username} is the catch-all.
func (h *Handlers) MountProfile(r chi.Router) {
	r.Get("/{username}", h.serveProfile)
}

// ----------------------------- profile ----------------------------------

func (h *Handlers) serveProfile(w http.ResponseWriter, r *http.Request) {
	rawName := chi.URLParam(r, "username")
	lower := strings.ToLower(rawName)

	// Defense in depth: a future top-level route that forgets to
	// register before the wildcard would otherwise resolve here. The
	// reserved-name list short-circuits with a 404.
	if authpkg.IsReserved(lower) {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}

	// S30: principals first. The single-row lookup decides whether
	// /{slug} dispatches to the user-profile renderer (existing) or
	// the org-profile renderer (this sprint). On miss, fall through
	// to username_redirects so renamed users keep redirecting.
	if p, err := orgs.Resolve(r.Context(), h.d.Pool, lower); err == nil {
		switch p.Kind {
		case orgs.PrincipalOrg:
			h.serveOrgProfile(w, r, p.ID)
			return
		case orgs.PrincipalUser:
			// Fall through to the user lookup below — keeps the
			// existing canonical-case redirect + suspension paths.
		}
	}

	// Try direct lookup. citext makes the comparison case-insensitive.
	user, err := h.q.GetUserByUsername(r.Context(), h.d.Pool, rawName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.tryRedirectOrNotFound(w, r, lower)
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "profile: lookup", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	// Canonical-case redirect. The DB stores the canonical casing; if
	// the URL differs, send a 301 so URLs are consistent.
	if rawName != user.Username {
		http.Redirect(w, r, "/"+user.Username, http.StatusMovedPermanently)
		return
	}

	if user.SuspendedAt.Valid || user.DeletedAt.Valid {
		h.renderUnavailable(w, r, user.Username)
		return
	}

	viewer := middleware.CurrentUserFromContext(r.Context())
	isSelf := viewer.ID != 0 && viewer.ID == user.ID

	// S26 Stars tab: `?tab=stars` switches to the user's starred-repos
	// view. Per-row visibility filtering happens in serveStarsTab so
	// private-repo stars only show to viewers who can see them.
	if r.URL.Query().Get("tab") == "stars" {
		h.serveStarsTab(w, r, user, viewer, isSelf)
		return
	}

	// Anonymous: ETag + small max-age. Self-view: no-cache.
	if isSelf {
		w.Header().Set("Cache-Control", "no-cache, private")
	} else {
		w.Header().Set("Cache-Control", "max-age=300")
	}

	avatarURL := fmt.Sprintf("/avatars/%s", url.PathEscape(user.Username))
	data := map[string]any{
		"Title":           user.DisplayName,
		"User":            user,
		"IsSelf":          isSelf,
		"AvatarURL":       avatarURL,
		"OGTitle":         user.DisplayName + " (@" + user.Username + ")",
		"OGDescription":   ogDescription(user),
		"OGImage":         avatarURL,
		"JoinedFormatted": user.CreatedAt.Time.Format("January 2, 2006"),
		"WebsiteSafe":     safeWebsite(user.Website),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.d.Render.RenderPage(w, r, "profile/view", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "profile: render", "error", err)
	}
}

// tryRedirectOrNotFound checks the username_redirects table and 301s on
// hit, otherwise renders the styled 404.
func (h *Handlers) tryRedirectOrNotFound(w http.ResponseWriter, r *http.Request, lower string) {
	row, err := h.q.LookupUsernameRedirect(r.Context(), h.d.Pool, lower)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}
	http.Redirect(w, r, "/"+row.Username, http.StatusMovedPermanently)
}

// renderUnavailable serves the dedicated suspended-user page. Distinct
// from 404 (would leak existence info) and from 200 (would imply normal
// profile).
func (h *Handlers) renderUnavailable(w http.ResponseWriter, r *http.Request, username string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusGone) // 410 — semantically "this resource is gone but we know it existed"
	if err := h.d.Render.RenderPage(w, r, "profile/suspended", map[string]any{
		"Title":    "Account unavailable",
		"Username": username,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "profile: render suspended", "error", err)
	}
}

// ------------------------------ avatar ----------------------------------

// serveAvatar resolves the username, then either streams the uploaded
// avatar from object storage or returns the deterministic SVG identicon.
//
// Implementation notes:
//   - Lookup-by-username happens on every request. At our scale this is
//     fine; if the avatar route becomes hot we can add an LRU.
//   - Suspended/deleted users get the identicon (NOT a 404) so the
//     suspended-page UX still has *something* to render in the header.
//   - Cache-Control: long max-age + immutable. Avatar contents are
//     content-addressed at upload time (S10 stores under
//     avatars/<owner>/<sha256>.<ext>) so the URL changes when the image
//     does, making "immutable" safe.
func (h *Handlers) serveAvatar(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	user, err := h.q.GetUserByUsername(r.Context(), h.d.Pool, username)
	if err != nil {
		// Don't 404 on missing user — silently fall through to the
		// identicon. Avatar URLs leak less existence info that way.
		writeIdenticon(w, r, username)
		return
	}
	if !user.AvatarObjectKey.Valid || user.AvatarObjectKey.String == "" {
		writeIdenticon(w, r, user.Username)
		return
	}
	if h.d.ObjectStore == nil {
		writeIdenticon(w, r, user.Username)
		return
	}
	rc, meta, err := h.d.ObjectStore.Get(r.Context(), user.AvatarObjectKey.String)
	if err != nil {
		writeIdenticon(w, r, user.Username)
		return
	}
	defer func() { _ = rc.Close() }()
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	if meta.ETag != "" {
		w.Header().Set("ETag", meta.ETag)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func writeIdenticon(w http.ResponseWriter, _ *http.Request, username string) {
	w.Header().Set("Content-Type", "image/svg+xml")
	// Identicons depend ONLY on the username; cache forever.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	// Defense in depth: forbid sniffing the body as HTML even though our
	// SVG body never echoes user input (it only hashes the username).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	//nolint:gosec // G705: body is server-generated SVG built from sha256(username).
	_, _ = w.Write([]byte(avatars.Identicon(username, 460)))
}

// ----------------------------- helpers ----------------------------------

func ogDescription(u usersdb.User) string {
	if u.Bio != "" {
		return u.Bio
	}
	return "@" + u.Username + " on shithub"
}

// safeWebsite returns u.Website only when it's an http(s) URL we can
// safely link out to. Anything else collapses to empty so the template
// doesn't render a clickable junk link.
func safeWebsite(s string) template.URL {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if u.Host == "" {
		return ""
	}
	return template.URL(u.String()) //nolint:gosec // schemes vetted above.
}

// ensure context import is used by static analysis even if a future
// refactor removes its only inline use.
var _ = context.Background

// serveOrgProfile renders /{org}. Pulls the org row + a small set of
// the org's visible repos. Visibility scoping defers to the caller's
// authentication state — a viewer that isn't a member sees only
// public repos.
func (h *Handlers) serveOrgProfile(w http.ResponseWriter, r *http.Request, orgID int64) {
	ctx := r.Context()
	org, err := orgsdb.New().GetOrgByID(ctx, h.d.Pool, orgID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}
	if org.DeletedAt.Valid {
		// Soft-deleted orgs render the same "unavailable" shell as
		// suspended/deleted users so the existence-leak posture is
		// uniform.
		h.renderUnavailable(w, r, string(org.Slug))
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	isOwner := false
	isMember := false
	if !viewer.IsAnonymous() {
		isOwner, _ = orgs.IsOwner(ctx, orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, org.ID, viewer.ID)
		isMember, _ = orgs.IsMember(ctx, orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, org.ID, viewer.ID)
	}

	// Org repo listing — small inline query to avoid widening sqlc
	// for one read. Members see private + public; non-members see
	// public only. Soft-deleted repos are excluded uniformly.
	visClause := "AND visibility = 'public'"
	args := []any{org.ID}
	if isMember {
		visClause = ""
	}
	rows, err := h.d.Pool.Query(ctx,
		`SELECT id, name, description, visibility::text
		   FROM repos
		  WHERE owner_org_id = $1 AND deleted_at IS NULL `+visClause+`
		  ORDER BY name ASC LIMIT 50`,
		args...)
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "orgs profile: list repos", "error", err)
	}
	type repoRow struct {
		Name, Description, Visibility string
	}
	var repos []repoRow
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id int64
			var rr repoRow
			if err := rows.Scan(&id, &rr.Name, &rr.Description, &rr.Visibility); err == nil {
				repos = append(repos, rr)
			}
		}
	}
	memberCount := 0
	{
		var n int64
		_ = h.d.Pool.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE org_id = $1`, org.ID).Scan(&n)
		memberCount = int(n)
	}

	_ = h.d.Render.RenderPage(w, r, "orgs/profile", map[string]any{
		"Title":       org.DisplayName,
		"Org":         org,
		"Repos":       repos,
		"MemberCount": memberCount,
		"IsOwner":     isOwner,
		"IsMember":    isMember,
	})
}

// avoid the unused-import lint when reposdb is only referenced in
// the inline raw query above.
var _ = reposdb.New
