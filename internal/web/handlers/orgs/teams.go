// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

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

type orgNavCounts struct {
	RepoCount   int64
	MemberCount int64
	TeamCount   int64
}

type teamAggregateCounts struct {
	MemberCount int64
	RepoCount   int64
	ChildCount  int64
}

type teamListItem struct {
	ID           int64
	Slug         string
	DisplayName  string
	Description  string
	Privacy      string
	ParentSlug   string
	Path         string
	MemberCount  int64
	RepoCount    int64
	ChildCount   int64
	IsSecret     bool
	HasParent    bool
	CreatedLabel string
}

type teamRepoCandidate struct {
	ID         int64
	Name       string
	Visibility string
}

type teamMemberCandidate struct {
	ID          int64
	Username    string
	DisplayName string
}

// teamsList renders /{org}/teams. GitHub keeps org teams member-only:
// visible teams are visible to org members, while secret teams are
// further limited to team members and org owners.
func (h *Handlers) teamsList(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.canSeeOrgTeams(w, r, org.ID, viewer) {
		return
	}
	all, err := orgsdb.New().ListTeamsForOrg(r.Context(), h.d.Pool, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "teams: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	visible := h.filterSecretTeams(r, all, org.ID, viewer)
	counts := h.teamAggregateCounts(r.Context(), org.ID)
	parentSlugs := teamParentSlugs(all)
	items := h.teamListItems(org, visible, counts, parentSlugs)
	visibleCount, secretCount := teamPrivacyCounts(items)
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	privacy := strings.TrimSpace(r.URL.Query().Get("privacy"))
	items = filterTeamListItems(items, query, privacy)
	isOwner := false
	if !viewer.IsAnonymous() {
		isOwner, _ = orgs.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
	}
	navCounts := h.orgNavCounts(r.Context(), org.ID, int64(len(visible)))
	if err := h.d.Render.RenderPage(w, r, "orgs/teams_list", map[string]any{
		"Title":          org.Slug + " · teams",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Org":            org,
		"AvatarURL":      "/avatars/" + url.PathEscape(string(org.Slug)),
		"ActiveOrgNav":   "teams",
		"Teams":          items,
		"TeamTotalCount": len(visible),
		"VisibleCount":   visibleCount,
		"SecretCount":    secretCount,
		"Query":          query,
		"PrivacyFilter":  privacy,
		"RepoCount":      navCounts.RepoCount,
		"MemberCount":    navCounts.MemberCount,
		"TeamCount":      navCounts.TeamCount,
		"IsOwner":        isOwner,
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
	if !h.canSeeOrgTeams(w, r, org.ID, viewer) {
		return
	}
	if !h.canSeeTeam(r, team, viewer) {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	q := orgsdb.New()
	members, _ := q.ListTeamMembers(r.Context(), h.d.Pool, team.ID)
	repos, _ := q.ListTeamRepoAccess(r.Context(), h.d.Pool, team.ID)
	children, _ := q.ListChildTeams(r.Context(), h.d.Pool, pgtype.Int8{Int64: team.ID, Valid: true})
	childItems := h.teamListItems(org, h.filterSecretTeams(r, children, org.ID, viewer),
		h.teamAggregateCounts(r.Context(), org.ID), teamParentSlugs(children))
	memberCandidates := h.teamMemberCandidates(r.Context(), org.ID, team.ID)
	repoCandidates := h.teamRepoCandidates(r.Context(), org.ID, team.ID)
	isOwner := false
	if !viewer.IsAnonymous() {
		isOwner, _ = orgs.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
	}
	navCounts := h.orgNavCounts(r.Context(), org.ID, -1)
	if err := h.d.Render.RenderPage(w, r, "orgs/team_view", map[string]any{
		"Title":            string(org.Slug) + "/" + string(team.Slug),
		"CSRFToken":        middleware.CSRFTokenForRequest(r),
		"Org":              org,
		"AvatarURL":        "/avatars/" + url.PathEscape(string(org.Slug)),
		"ActiveOrgNav":     "teams",
		"Team":             team,
		"TeamDisplayName":  teamDisplayName(team),
		"TeamPath":         h.teamPath(org, team),
		"TeamPrivacy":      string(team.Privacy),
		"TeamIsSecret":     team.Privacy == orgsdb.TeamPrivacySecret,
		"ChildTeams":       childItems,
		"Members":          members,
		"MemberCandidates": memberCandidates,
		"Repos":            repos,
		"RepoCandidates":   repoCandidates,
		"RepoCount":        navCounts.RepoCount,
		"MemberCount":      navCounts.MemberCount,
		"TeamCount":        navCounts.TeamCount,
		"IsOwner":          isOwner,
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
	action := r.PostFormValue("action")
	uid, ok := h.userIDFromTeamMemberForm(r)
	if !ok {
		http.Redirect(w, r, h.teamPath(org, team), http.StatusSeeOther)
		return
	}
	if action != "remove" && !h.userIsOrgMember(r.Context(), org.ID, uid) {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
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
	repoID, err := h.repoIDFromTeamForm(r, org.ID)
	if err != nil || repoID == 0 {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	if !h.repoBelongsToOrg(r.Context(), org.ID, repoID) {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
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

func (h *Handlers) canSeeOrgTeams(w http.ResponseWriter, r *http.Request, orgID int64, viewer middleware.CurrentUser) bool {
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return false
	}
	if viewer.IsSiteAdmin {
		return true
	}
	isMember, _ := orgs.IsMember(r.Context(), h.deps(), orgID, viewer.ID)
	if !isMember {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return false
	}
	return true
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

func (h *Handlers) userIDFromTeamMemberForm(r *http.Request) (int64, bool) {
	if raw := strings.TrimSpace(r.PostFormValue("user_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && id != 0 {
			return id, true
		}
		return 0, false
	}
	username := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	if username == "" {
		return 0, false
	}
	return h.userIDByUsername(r, username)
}

func (h *Handlers) userIsOrgMember(ctx context.Context, orgID, userID int64) bool {
	var exists bool
	err := h.d.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM org_members WHERE org_id = $1 AND user_id = $2)`,
		orgID, userID,
	).Scan(&exists)
	return err == nil && exists
}

// canSeeTeam decides whether the viewer is allowed to see a team's members
// and repositories. canSeeOrgTeams has already enforced org membership;
// visible teams are readable to those members, while secret teams require
// team membership or org ownership.
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

// filterSecretTeams strips secret teams the viewer can't see after the
// caller has already established org-team-page visibility.
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

func (h *Handlers) orgNavCounts(ctx context.Context, orgID int64, visibleTeamCount int64) orgNavCounts {
	var counts orgNavCounts
	_ = h.d.Pool.QueryRow(ctx, `SELECT count(*) FROM repos WHERE owner_org_id = $1 AND deleted_at IS NULL`, orgID).Scan(&counts.RepoCount)
	_ = h.d.Pool.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE org_id = $1`, orgID).Scan(&counts.MemberCount)
	if visibleTeamCount >= 0 {
		counts.TeamCount = visibleTeamCount
	} else {
		_ = h.d.Pool.QueryRow(ctx, `SELECT count(*) FROM teams WHERE org_id = $1`, orgID).Scan(&counts.TeamCount)
	}
	return counts
}

func (h *Handlers) teamAggregateCounts(ctx context.Context, orgID int64) map[int64]teamAggregateCounts {
	rows, err := h.d.Pool.Query(ctx, `
		SELECT t.id,
		       count(DISTINCT tm.user_id)::bigint AS member_count,
		       count(DISTINCT tra.repo_id)::bigint AS repo_count,
		       count(DISTINCT child.id)::bigint AS child_count
		  FROM teams t
		  LEFT JOIN team_members tm ON tm.team_id = t.id
		  LEFT JOIN team_repo_access tra ON tra.team_id = t.id
		  LEFT JOIN teams child ON child.parent_team_id = t.id
		 WHERE t.org_id = $1
		 GROUP BY t.id`, orgID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "teams: counts", "org_id", orgID, "error", err)
		return nil
	}
	defer rows.Close()
	out := map[int64]teamAggregateCounts{}
	for rows.Next() {
		var id int64
		var c teamAggregateCounts
		if err := rows.Scan(&id, &c.MemberCount, &c.RepoCount, &c.ChildCount); err == nil {
			out[id] = c
		}
	}
	return out
}

func (h *Handlers) teamListItems(org orgsdb.Org, teams []orgsdb.Team, counts map[int64]teamAggregateCounts, parentSlugs map[int64]string) []teamListItem {
	out := make([]teamListItem, 0, len(teams))
	for _, team := range teams {
		c := counts[team.ID]
		parentSlug := ""
		if team.ParentTeamID.Valid {
			parentSlug = parentSlugs[team.ParentTeamID.Int64]
		}
		out = append(out, teamListItem{
			ID:           team.ID,
			Slug:         string(team.Slug),
			DisplayName:  teamDisplayName(team),
			Description:  team.Description,
			Privacy:      string(team.Privacy),
			ParentSlug:   parentSlug,
			Path:         h.teamPath(org, team),
			MemberCount:  c.MemberCount,
			RepoCount:    c.RepoCount,
			ChildCount:   c.ChildCount,
			IsSecret:     team.Privacy == orgsdb.TeamPrivacySecret,
			HasParent:    team.ParentTeamID.Valid,
			CreatedLabel: team.CreatedAt.Time.Format("Jan 2, 2006"),
		})
	}
	return out
}

func teamParentSlugs(teams []orgsdb.Team) map[int64]string {
	if len(teams) == 0 {
		return nil
	}
	byID := make(map[int64]string, len(teams))
	for _, team := range teams {
		byID[team.ID] = string(team.Slug)
	}
	return byID
}

func teamDisplayName(team orgsdb.Team) string {
	if strings.TrimSpace(team.DisplayName) != "" {
		return team.DisplayName
	}
	return string(team.Slug)
}

func teamPrivacyCounts(items []teamListItem) (visibleCount, secretCount int) {
	for _, item := range items {
		if item.IsSecret {
			secretCount++
		} else {
			visibleCount++
		}
	}
	return visibleCount, secretCount
}

func filterTeamListItems(items []teamListItem, query, privacy string) []teamListItem {
	query = strings.ToLower(strings.TrimSpace(query))
	privacy = strings.ToLower(strings.TrimSpace(privacy))
	if query == "" && privacy == "" {
		return items
	}
	out := make([]teamListItem, 0, len(items))
	for _, item := range items {
		if privacy == "visible" && item.IsSecret {
			continue
		}
		if privacy == "secret" && !item.IsSecret {
			continue
		}
		if query != "" {
			haystack := strings.ToLower(item.Slug + " " + item.DisplayName + " " + item.Description)
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func (h *Handlers) teamRepoCandidates(ctx context.Context, orgID, teamID int64) []teamRepoCandidate {
	rows, err := h.d.Pool.Query(ctx, `
		SELECT r.id, r.name, r.visibility::text
		  FROM repos r
		  LEFT JOIN team_repo_access a
		    ON a.repo_id = r.id AND a.team_id = $2
		 WHERE r.owner_org_id = $1
		   AND r.deleted_at IS NULL
		   AND a.repo_id IS NULL
		 ORDER BY lower(r.name)
		 LIMIT 100`, orgID, teamID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "teams: repo candidates", "org_id", orgID, "team_id", teamID, "error", err)
		return nil
	}
	defer rows.Close()
	out := []teamRepoCandidate{}
	for rows.Next() {
		var item teamRepoCandidate
		if err := rows.Scan(&item.ID, &item.Name, &item.Visibility); err == nil {
			out = append(out, item)
		}
	}
	return out
}

func (h *Handlers) teamMemberCandidates(ctx context.Context, orgID, teamID int64) []teamMemberCandidate {
	rows, err := h.d.Pool.Query(ctx, `
		SELECT u.id, u.username, u.display_name
		  FROM org_members om
		  JOIN users u ON u.id = om.user_id
		  LEFT JOIN team_members tm
		    ON tm.team_id = $2 AND tm.user_id = u.id
		 WHERE om.org_id = $1
		   AND u.deleted_at IS NULL
		   AND tm.user_id IS NULL
		 ORDER BY lower(u.username)
		 LIMIT 100`, orgID, teamID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "teams: member candidates", "org_id", orgID, "team_id", teamID, "error", err)
		return nil
	}
	defer rows.Close()
	out := []teamMemberCandidate{}
	for rows.Next() {
		var item teamMemberCandidate
		if err := rows.Scan(&item.ID, &item.Username, &item.DisplayName); err == nil {
			out = append(out, item)
		}
	}
	return out
}

func (h *Handlers) repoIDFromTeamForm(r *http.Request, orgID int64) (int64, error) {
	if raw := strings.TrimSpace(r.PostFormValue("repo_id")); raw != "" {
		return strconv.ParseInt(raw, 10, 64)
	}
	repoName := strings.TrimSpace(r.PostFormValue("repo_name"))
	if repoName == "" {
		return 0, strconv.ErrSyntax
	}
	var id int64
	err := h.d.Pool.QueryRow(
		r.Context(),
		`SELECT id FROM repos WHERE owner_org_id = $1 AND name = $2 AND deleted_at IS NULL`,
		orgID, repoName,
	).Scan(&id)
	return id, err
}

func (h *Handlers) repoBelongsToOrg(ctx context.Context, orgID, repoID int64) bool {
	var exists bool
	err := h.d.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM repos WHERE id = $1 AND owner_org_id = $2 AND deleted_at IS NULL)`,
		repoID, orgID,
	).Scan(&exists)
	return err == nil && exists
}

func (h *Handlers) teamPath(org orgsdb.Org, team orgsdb.Team) string {
	return "/" + string(org.Slug) + "/teams/" + string(team.Slug)
}
