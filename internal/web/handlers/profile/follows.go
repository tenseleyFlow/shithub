// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/social"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const followsPageSize = 50

type followState struct {
	FollowersCount int64
	FollowingCount int64
	IsFollowing    bool
}

type followListItem struct {
	Kind        string
	Username    string
	DisplayName string
	AvatarURL   string
	URL         string
	FollowedAt  string
	// IsPro is set for user rows whose plan is the Pro user plan. Org
	// rows always carry false. PRO-EXT_SR2-15 — the template uses this
	// to decide whether to render the pro-badge next to the handle.
	IsPro bool
}

func (h *Handlers) socialDeps() social.Deps {
	return social.Deps{
		Pool:    h.d.Pool,
		Limiter: h.d.Limiter,
		Logger:  h.d.Logger,
		Audit:   h.d.Audit,
	}
}

func (h *Handlers) profileFollow(w http.ResponseWriter, r *http.Request) {
	h.followAction(w, r, true)
}

func (h *Handlers) profileUnfollow(w http.ResponseWriter, r *http.Request) {
	h.followAction(w, r, false)
}

func (h *Handlers) followAction(w http.ResponseWriter, r *http.Request, follow bool) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	rawName := chi.URLParam(r, "username")
	lower := strings.ToLower(rawName)
	if authpkg.IsReserved(lower) {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}

	if p, err := orgs.Resolve(r.Context(), h.d.Pool, lower); err == nil {
		switch p.Kind {
		case orgs.PrincipalOrg:
			h.followOrgAction(w, r, viewer, p.ID, follow)
			return
		case orgs.PrincipalUser:
			h.followUserAction(w, r, viewer, rawName, follow)
			return
		}
	}
	h.followUserAction(w, r, viewer, rawName, follow)
}

func (h *Handlers) followUserAction(w http.ResponseWriter, r *http.Request, viewer middleware.CurrentUser, rawName string, follow bool) {
	target, err := h.q.GetUserByUsername(r.Context(), h.d.Pool, rawName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "profile follow: lookup user", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if target.SuspendedAt.Valid || target.DeletedAt.Valid {
		h.renderUnavailable(w, r, target.Username)
		return
	}
	var actionErr error
	if follow {
		actionErr = social.FollowUser(r.Context(), h.socialDeps(), viewer.ID, target.ID)
	} else {
		actionErr = social.UnfollowUser(r.Context(), h.socialDeps(), viewer.ID, target.ID)
	}
	if actionErr != nil {
		h.handleFollowError(w, r, actionErr)
		return
	}
	redirectAfterProfileAction(w, r, "/"+target.Username)
}

func (h *Handlers) followOrgAction(w http.ResponseWriter, r *http.Request, viewer middleware.CurrentUser, orgID int64, follow bool) {
	org, err := orgsdb.New().GetOrgByID(r.Context(), h.d.Pool, orgID)
	if err != nil || org.DeletedAt.Valid {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}
	if org.SuspendedAt.Valid {
		h.d.Render.HTTPError(w, r, http.StatusGone, string(org.Slug))
		return
	}
	var actionErr error
	if follow {
		actionErr = social.FollowOrg(r.Context(), h.socialDeps(), viewer.ID, org.ID)
	} else {
		actionErr = social.UnfollowOrg(r.Context(), h.socialDeps(), viewer.ID, org.ID)
	}
	if actionErr != nil {
		h.handleFollowError(w, r, actionErr)
		return
	}
	redirectAfterProfileAction(w, r, "/"+org.Slug)
}

func (h *Handlers) handleFollowError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, social.ErrNotLoggedIn):
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	case errors.Is(err, social.ErrCannotFollowSelf):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "cannot follow yourself")
	case errors.Is(err, social.ErrFollowRateLimit):
		h.d.Render.HTTPError(w, r, http.StatusTooManyRequests, "rate limit")
	default:
		h.d.Logger.ErrorContext(r.Context(), "profile follow", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}

func redirectAfterProfileAction(w http.ResponseWriter, r *http.Request, fallback string) {
	dest := fallback
	if returnTo := strings.TrimSpace(r.PostFormValue("return_to")); safeProfileReturnTo(returnTo) {
		dest = returnTo
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func safeProfileReturnTo(path string) bool {
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return false
	}
	u, err := url.Parse(path)
	return err == nil && !u.IsAbs() && u.Host == "" && strings.HasPrefix(u.Path, "/")
}

func (h *Handlers) userFollowState(ctx context.Context, userID int64, viewer middleware.CurrentUser) followState {
	q := socialdb.New()
	var out followState
	out.FollowersCount, _ = q.CountFollowersForUser(ctx, h.d.Pool, pgtype.Int8{Int64: userID, Valid: true})
	out.FollowingCount, _ = q.CountFollowingForUser(ctx, h.d.Pool, userID)
	if !viewer.IsAnonymous() && viewer.ID != userID {
		out.IsFollowing, _ = social.IsFollowingUser(ctx, h.socialDeps(), viewer.ID, userID)
	}
	return out
}

func (h *Handlers) orgFollowState(ctx context.Context, orgID int64, viewer middleware.CurrentUser) followState {
	q := socialdb.New()
	var out followState
	out.FollowersCount, _ = q.CountFollowersForOrg(ctx, h.d.Pool, pgtype.Int8{Int64: orgID, Valid: true})
	if !viewer.IsAnonymous() {
		out.IsFollowing, _ = social.IsFollowingOrg(ctx, h.socialDeps(), viewer.ID, orgID)
	}
	return out
}

func (h *Handlers) serveFollowersTab(w http.ResponseWriter, r *http.Request, user usersdb.User, viewer middleware.CurrentUser, isSelf bool) {
	page := pageFromRequest(r)
	rows, err := socialdb.New().ListFollowersForUser(r.Context(), h.d.Pool, socialdb.ListFollowersForUserParams{
		FolloweeUserID: pgtype.Int8{Int64: user.ID, Valid: true},
		Limit:          followsPageSize,
		Offset:         int32((page - 1) * followsPageSize),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "profile followers: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	state := h.userFollowState(r.Context(), user.ID, viewer)
	items := make([]followListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, userFollowListItem(row.Username, row.DisplayName, row.FollowedAt, row.Plan))
	}
	h.renderFollowsTab(w, r, user, isSelf, "followers", state, items, page)
}

func (h *Handlers) serveFollowingTab(w http.ResponseWriter, r *http.Request, user usersdb.User, viewer middleware.CurrentUser, isSelf bool) {
	userRows, err := socialdb.New().ListFollowingUsersForUser(r.Context(), h.d.Pool, socialdb.ListFollowingUsersForUserParams{
		FollowerUserID: user.ID,
		Limit:          followsPageSize,
		Offset:         0,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "profile following users: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	orgRows, err := socialdb.New().ListFollowingOrgsForUser(r.Context(), h.d.Pool, socialdb.ListFollowingOrgsForUserParams{
		FollowerUserID: user.ID,
		Limit:          followsPageSize,
		Offset:         0,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "profile following orgs: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	state := h.userFollowState(r.Context(), user.ID, viewer)
	items := make([]followListItem, 0, len(userRows)+len(orgRows))
	for _, row := range userRows {
		items = append(items, userFollowListItem(row.Username, row.DisplayName, row.FollowedAt, row.Plan))
	}
	for _, row := range orgRows {
		items = append(items, orgFollowListItem(row.Slug, row.DisplayName, row.FollowedAt))
	}
	h.renderFollowsTab(w, r, user, isSelf, "following", state, items, 1)
}

func (h *Handlers) renderFollowsTab(w http.ResponseWriter, r *http.Request, user usersdb.User, isSelf bool, active string, state followState, items []followListItem, page int) {
	displayName := user.DisplayName
	if displayName == "" {
		displayName = user.Username
	}
	// PRO-EXT_SR2-15: derive the Pro-username set from items so the
	// template can render the Pro badge next to Pro users (matches
	// every other user-bearing surface).
	proUsernames := make(map[string]bool, len(items))
	for _, it := range items {
		if it.IsPro {
			proUsernames[it.Username] = true
		}
	}
	data := map[string]any{
		"Title":          followTabTitle(active) + " · " + user.Username,
		"User":           user,
		"DisplayName":    displayName,
		"IsSelf":         isSelf,
		"AvatarURL":      "/avatars/" + url.PathEscape(user.Username),
		"Tabs":           h.tabCounts(r.Context(), user.ID, middleware.CurrentUserFromContext(r.Context())),
		"ActiveTab":      active,
		"FollowersCount": state.FollowersCount,
		"FollowingCount": state.FollowingCount,
		"Items":          items,
		"ProUsernames":   proUsernames,
		"Page":           page,
		"HasPrev":        page > 1,
		"HasNext":        len(items) == followsPageSize,
	}
	if err := h.d.Render.RenderPage(w, r, "profile/follows_tab", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "profile follows: render", "error", err)
	}
}

func followTabTitle(active string) string {
	switch active {
	case "followers":
		return "Followers"
	case "following":
		return "Following"
	default:
		return "People"
	}
}

func pageFromRequest(r *http.Request) int {
	v, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if v < 1 {
		return 1
	}
	return v
}

func userFollowListItem(username, displayName string, followedAt pgtype.Timestamptz, plan socialdb.UserPlan) followListItem {
	return followListItem{
		Kind: "user", Username: username, DisplayName: displayName,
		AvatarURL: "/avatars/" + url.PathEscape(username), URL: "/" + username,
		FollowedAt: followedAt.Time.Format("Jan 2, 2006"),
		IsPro:      plan == socialdb.UserPlanPro,
	}
}

func orgFollowListItem(slug, displayName string, followedAt pgtype.Timestamptz) followListItem {
	return followListItem{
		Kind: "org", Username: slug, DisplayName: displayName,
		AvatarURL: "/avatars/" + url.PathEscape(slug), URL: "/" + slug,
		FollowedAt: followedAt.Time.Format("Jan 2, 2006"),
	}
}
