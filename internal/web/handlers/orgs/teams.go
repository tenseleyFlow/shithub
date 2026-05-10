// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountTeams registers the per-org team surface. Read paths are
// public-but-filtered (secret teams hidden from non-members);
// mutations are owner-gated inside each handler.
func (h *Handlers) MountTeams(r chi.Router) {
	r.Get("/{org}/teams", h.teamsList)
	r.Post("/{org}/teams", h.teamCreate)
	r.Get("/{org}/teams/{teamSlug}", h.teamView)
	r.Post("/{org}/teams/{teamSlug}/members", h.teamMemberAddRemove)
	r.Post("/{org}/teams/{teamSlug}/repos", h.teamRepoGrant)
}

// teamsList renders /{org}/teams. Filters secret teams out for
// non-members + non-owners.
func (h *Handlers) teamsList(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	all, err := orgsdb.New().ListTeamsForOrg(r.Context(), h.d.Pool, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "teams: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	visible := h.filterSecretTeams(r, all, org.ID, viewer)
	isOwner := false
	if !viewer.IsAnonymous() {
		isOwner, _ = orgs.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
	}
	if err := h.d.Render.RenderPage(w, r, "orgs/teams_list", map[string]any{
		"Title":     org.Slug + " · teams",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Org":       org,
		"Teams":     visible,
		"IsOwner":   isOwner,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: render", "tpl", "orgs/teams_list", "error", err)
	}
}

// teamCreate handles POST /{org}/teams. Owner-only.
func (h *Handlers) teamCreate(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.requireOrgOwner(w, r, org.ID, viewer) {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	parentID, _ := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("parent_team_id")), 10, 64)
	_, err := orgs.CreateTeam(r.Context(), h.deps(), orgs.CreateTeamParams{
		OrgID:           org.ID,
		Slug:            strings.TrimSpace(r.PostFormValue("slug")),
		DisplayName:     strings.TrimSpace(r.PostFormValue("display_name")),
		Description:     strings.TrimSpace(r.PostFormValue("description")),
		ParentTeamID:    parentID,
		Privacy:         strings.TrimSpace(r.PostFormValue("privacy")),
		CreatedByUserID: viewer.ID,
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "teams: create",
			"org", org.Slug, "error", err)
	}
	http.Redirect(w, r, "/"+string(org.Slug)+"/teams", http.StatusSeeOther)
}

// teamView renders /{org}/teams/{teamSlug}. Members + repo access.
// Secret teams 404 for non-members + non-owners.
func (h *Handlers) teamView(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	team, ok := h.teamFromSlug(w, r, org.ID)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.canSeeTeam(r, team, viewer) {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	q := orgsdb.New()
	members, _ := q.ListTeamMembers(r.Context(), h.d.Pool, team.ID)
	repos, _ := q.ListTeamRepoAccess(r.Context(), h.d.Pool, team.ID)
	isOwner := false
	if !viewer.IsAnonymous() {
		isOwner, _ = orgs.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
	}
	if err := h.d.Render.RenderPage(w, r, "orgs/team_view", map[string]any{
		"Title":     string(org.Slug) + "/" + string(team.Slug),
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Org":       org,
		"Team":      team,
		"Members":   members,
		"Repos":     repos,
		"IsOwner":   isOwner,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: render", "tpl", "orgs/team_view", "error", err)
	}
}

// teamMemberAddRemove handles POST .../members. Form action=add|remove.
// Both branches are owner-only; the orchestrator keeps idempotency.
func (h *Handlers) teamMemberAddRemove(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	team, ok := h.teamFromSlug(w, r, org.ID)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.requireOrgOwner(w, r, org.ID, viewer) {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	action := r.PostFormValue("action")
	if username == "" {
		http.Redirect(w, r, h.teamPath(org, team), http.StatusSeeOther)
		return
	}
	uid, ok := h.userIDByUsername(r, username)
	if !ok {
		http.Redirect(w, r, h.teamPath(org, team), http.StatusSeeOther)
		return
	}
	switch action {
	case "remove":
		_ = orgs.RemoveTeamMember(r.Context(), h.deps(), team.ID, uid)
	default:
		role := r.PostFormValue("role")
		_ = orgs.AddTeamMember(r.Context(), h.deps(), team.ID, uid, viewer.ID, role)
	}
	http.Redirect(w, r, h.teamPath(org, team), http.StatusSeeOther)
}

// teamRepoGrant handles POST .../repos. Form expects repo_id + role,
// or repo_id + action=remove. Owner-only.
func (h *Handlers) teamRepoGrant(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	team, ok := h.teamFromSlug(w, r, org.ID)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.requireOrgOwner(w, r, org.ID, viewer) {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	repoID, err := strconv.ParseInt(r.PostFormValue("repo_id"), 10, 64)
	if err != nil || repoID == 0 {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	if r.PostFormValue("action") == "remove" {
		_ = orgs.RevokeTeamRepoAccess(r.Context(), h.deps(), team.ID, repoID)
	} else {
		_ = orgs.GrantTeamRepoAccess(r.Context(), h.deps(), team.ID, repoID, viewer.ID,
			r.PostFormValue("role"))
	}
	http.Redirect(w, r, h.teamPath(org, team), http.StatusSeeOther)
}

// ─── small helpers ─────────────────────────────────────────────────

func (h *Handlers) teamFromSlug(w http.ResponseWriter, r *http.Request, orgID int64) (orgsdb.Team, bool) {
	slug := chi.URLParam(r, "teamSlug")
	row, err := orgsdb.New().GetTeamByOrgAndSlug(r.Context(), h.d.Pool, orgsdb.GetTeamByOrgAndSlugParams{
		OrgID: orgID, Slug: slug,
	})
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return orgsdb.Team{}, false
	}
	return row, true
}

func (h *Handlers) requireOrgOwner(w http.ResponseWriter, r *http.Request, orgID int64, viewer middleware.CurrentUser) bool {
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
		return false
	}
	// Suspended actors get the same 403 as non-owners. Mirrors the
	// suspended gate the policy package enforces on every other
	// mutation surface — this gate doesn't go through policy.Can yet
	// (the org/team actions aren't in the policy enum), so we
	// short-circuit here (SR2 C4). Same shape as SR1 C1 fix.
	if viewer.IsSuspended {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return false
	}
	owner, _ := orgs.IsOwner(r.Context(), h.deps(), orgID, viewer.ID)
	if !owner {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return false
	}
	return true
}

func (h *Handlers) userIDByUsername(r *http.Request, username string) (int64, bool) {
	var id int64
	err := h.d.Pool.QueryRow(
		r.Context(),
		`SELECT id FROM users WHERE username = $1 AND deleted_at IS NULL`,
		username,
	).Scan(&id)
	if err != nil {
		return 0, false
	}
	return id, true
}

// canSeeTeam decides whether the viewer is allowed to see a team's
// members + repos. Visible teams are public to all org members and
// their basic info is public to everyone; secret teams are private
// to (team members ∪ org owners). For simplicity the page-render
// check requires ANY membership/owner; the list page does the same
// filter when assembling the visible set.
func (h *Handlers) canSeeTeam(r *http.Request, team orgsdb.Team, viewer middleware.CurrentUser) bool {
	if team.Privacy == orgsdb.TeamPrivacyVisible {
		return true
	}
	if viewer.IsAnonymous() {
		return false
	}
	// Org owner sees all.
	if owner, _ := orgs.IsOwner(r.Context(), h.deps(), team.OrgID, viewer.ID); owner {
		return true
	}
	// Team member? (SR2 M2 + M3: was an inline EXISTS preceded by a
	// wasted ListTeamMembers call whose result was dropped with `_`.)
	member, err := orgsdb.New().IsTeamMember(r.Context(), h.d.Pool, orgsdb.IsTeamMemberParams{
		TeamID: team.ID, UserID: viewer.ID,
	})
	if err != nil {
		return false
	}
	return member
}

// filterSecretTeams strips secret teams the viewer can't see.
func (h *Handlers) filterSecretTeams(r *http.Request, all []orgsdb.Team, orgID int64, viewer middleware.CurrentUser) []orgsdb.Team {
	if len(all) == 0 {
		return all
	}
	out := make([]orgsdb.Team, 0, len(all))
	isOwner := false
	if !viewer.IsAnonymous() {
		isOwner, _ = orgs.IsOwner(r.Context(), h.deps(), orgID, viewer.ID)
	}
	for _, t := range all {
		if t.Privacy == orgsdb.TeamPrivacyVisible || isOwner {
			out = append(out, t)
			continue
		}
		// Secret + non-owner: only show when the viewer is a member.
		if viewer.IsAnonymous() {
			continue
		}
		// SR2 M2: was an inline EXISTS query.
		member, err := orgsdb.New().IsTeamMember(r.Context(), h.d.Pool, orgsdb.IsTeamMemberParams{
			TeamID: t.ID, UserID: viewer.ID,
		})
		if err == nil && member {
			out = append(out, t)
		}
	}
	return out
}

func (h *Handlers) teamPath(org orgsdb.Org, team orgsdb.Team) string {
	return "/" + string(org.Slug) + "/teams/" + string(team.Slug)
}

// ensure pgx is referenced when the rest of the file's imports
// settle (avoids a "imported and not used" if a future refactor
// drops the only inline pgx use).
var _ = pgx.ErrNoRows

// errTeamNotFound is reserved for the future; surfaced via
// orgs.ErrTeamNotFound when needed.
var _ = errors.New
