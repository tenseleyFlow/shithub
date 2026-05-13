// SPDX-License-Identifier: AGPL-3.0-or-later

// Package profile owns the read-only public profile handlers:
// /{username}, /orgs/{org}/repositories, and /avatars/{username}.
// Edit-profile is S10.
//
// Route ordering is critical here: the wildcard /{username} catches any
// path the chi router didn't already match. The reserved-name list is the
// second line of defense if a future top-level route is added but not
// registered before the wildcard. The route-audit test in
// internal/web/handlers/handlers_test.go enforces both lines of defense.
package profile

import (
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
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/avatars"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps wires the profile handlers.
type Deps struct {
	Logger *slog.Logger
	Render *render.Renderer
	Pool   *pgxpool.Pool
	// RepoFS is optional for profile tests, but production passes it so
	// org overview repo rows can render commit-activity sparklines.
	RepoFS *storage.RepoFS
	// ObjectStore is used to stream uploaded avatars. May be nil in tests
	// or when S3 is not configured — falls back to identicon.
	ObjectStore storage.ObjectStore
	Limiter     *throttle.Limiter
	Audit       *audit.Recorder
	// BillingEnforce carries PRO07's per-feature enforcement flags.
	// Zero value (all false) keeps profile-pin gating in report-only mode:
	// Free users over the cap continue to hit a generic 400, the would-deny
	// is logged for the soak. When UserProfilePinsBeyondFree flips true
	// the same overflow returns 402 with an upgrade banner.
	BillingEnforce config.EnforceConfig
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
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireUser)
		r.Post("/{username}/contribution-settings", h.contributionSettingsUpdate)
		r.Post("/{username}/follow", h.profileFollow)
		r.Post("/{username}/unfollow", h.profileUnfollow)
		r.Post("/{username}/pins", h.pinsUpdate)
	})
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

	// Sub-nav dispatch. Overview is the default; ?tab=repositories
	// and ?tab=stars take their own renderers (each one is
	// visibility-filtered independently).
	switch r.URL.Query().Get("tab") {
	case "followers":
		h.serveFollowersTab(w, r, user, viewer, isSelf)
		return
	case "following":
		h.serveFollowingTab(w, r, user, viewer, isSelf)
		return
	case "stars":
		h.serveStarsTab(w, r, user, viewer, isSelf)
		return
	case "repositories":
		h.serveRepositoriesTab(w, r, user, viewer, isSelf)
		return
	}

	// Anonymous: ETag + small max-age. Self-view: no-cache.
	if isSelf {
		w.Header().Set("Cache-Control", "no-cache, private")
	} else {
		w.Header().Set("Cache-Control", "max-age=300")
	}

	avatarURL := fmt.Sprintf("/avatars/%s", url.PathEscape(user.Username))
	tabs := h.tabCounts(r.Context(), user.ID, viewer)
	followState := h.userFollowState(r.Context(), user.ID, viewer)
	visibleRepos := h.visibleUserRepos(r.Context(), user.ID, viewer)
	pinnedRepos, pinCandidates := h.userPinData(r.Context(), user)
	userPinsCap, _ := h.userProfilePinCap(r.Context(), user.ID)
	readme, hasReadme := h.profileReadme(r.Context(), user, viewer)
	displayName := user.DisplayName
	if displayName == "" {
		displayName = user.Username
	}
	data := map[string]any{
		"Title":                      displayName,
		"User":                       user,
		"DisplayName":                displayName,
		"IsSelf":                     isSelf,
		"AvatarURL":                  avatarURL,
		"OGTitle":                    displayName + " (@" + user.Username + ")",
		"OGDescription":              ogDescription(user),
		"OGImage":                    avatarURL,
		"JoinedFormatted":            user.CreatedAt.Time.Format("January 2, 2006"),
		"WebsiteSafe":                safeWebsite(user.Website),
		"Tabs":                       tabs,
		"ActiveTab":                  "overview",
		"FollowersCount":             followState.FollowersCount,
		"FollowingCount":             followState.FollowingCount,
		"IsFollowing":                followState.IsFollowing,
		"FollowAction":               "/" + url.PathEscape(user.Username) + "/follow",
		"UnfollowAction":             "/" + url.PathEscape(user.Username) + "/unfollow",
		"ReturnTo":                   r.URL.RequestURI(),
		"VisibleRepoCount":           len(visibleRepos),
		"Orgs":                       h.profileOrganizations(r.Context(), user.ID),
		"ProfileReadme":              readme,
		"HasProfileReadme":           hasReadme,
		"Contributions":              h.contributionCalendar(r.Context(), user, viewer, r.URL.Query()),
		"PinnedRepos":                pinnedRepos,
		"PinCandidates":              pinCandidates,
		"PinsRemaining":              profilePinsRemaining(pinCandidates, userPinsCap),
		"CanCustomizePins":           isSelf,
		"PinsAction":                 "/" + url.PathEscape(user.Username) + "/pins",
		"ContributionSettingsAction": "/" + url.PathEscape(user.Username) + "/contribution-settings",
		"ContributionSettingsReturn": r.URL.RequestURI(),
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

// serveAvatar resolves the slug, then either streams the uploaded
// user/org avatar from object storage or returns the deterministic SVG
// identicon.
//
// Implementation notes:
//   - Lookup-by-username happens on every request. At our scale this is
//     fine; if the avatar route becomes hot we can add an LRU.
//   - Missing, suspended, or deleted principals get the identicon (NOT a
//     404) so avatar URLs leak less existence state.
//   - Cache-Control: long max-age + immutable. Avatar contents are
//     content-addressed at upload time so the URL changes when the image
//     changes, making "immutable" safe.
func (h *Handlers) serveAvatar(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "username")
	key, seed := h.avatarKeyForSlug(r, slug)
	if key == "" {
		writeIdenticon(w, r, seed)
		return
	}
	if h.d.ObjectStore == nil {
		writeIdenticon(w, r, seed)
		return
	}
	rc, meta, err := h.d.ObjectStore.Get(r.Context(), key)
	if err != nil {
		writeIdenticon(w, r, seed)
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

func (h *Handlers) avatarKeyForSlug(r *http.Request, slug string) (string, string) {
	user, err := h.q.GetUserByUsername(r.Context(), h.d.Pool, slug)
	if err == nil {
		if user.AvatarObjectKey.Valid {
			return user.AvatarObjectKey.String, user.Username
		}
		return "", user.Username
	}
	org, err := orgsdb.New().GetOrgBySlug(r.Context(), h.d.Pool, slug)
	if err == nil {
		if org.AvatarObjectKey.Valid {
			return org.AvatarObjectKey.String, org.Slug
		}
		return "", org.Slug
	}
	return "", slug
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
