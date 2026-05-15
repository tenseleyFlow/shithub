// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	policydb "github.com/tenseleyFlow/shithub/internal/auth/policy/sqlc"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountSettingsGeneral wires the General-tab routes plus the placeholder
// pages for tabs that depend on later sprints (webhooks/keys/notifications/
// tags). Caller wraps with RequireUser; per-route policy gates inside.
func (h *Handlers) MountSettingsGeneral(r chi.Router) {
	r.Get("/{owner}/{repo}/settings/general", h.settingsGeneral)
	r.Post("/{owner}/{repo}/settings/general", h.settingsGeneralUpdate)
	r.Post("/{owner}/{repo}/settings/source-remote", h.settingsSourceRemoteUpdate)
	r.Post("/{owner}/{repo}/settings/merges", h.settingsMergeUpdate)
	r.Get("/{owner}/{repo}/settings/access", h.settingsAccess)
	r.Post("/{owner}/{repo}/settings/access/collaborators", h.settingsCollabUpsert)
	r.Post("/{owner}/{repo}/settings/access/collaborators/remove", h.settingsCollabRemove)
	r.Post("/{owner}/{repo}/settings/access/teams", h.settingsTeamGrant)
	r.Post("/{owner}/{repo}/settings/access/teams/revoke", h.settingsTeamRevoke)
	r.Get("/{owner}/{repo}/settings/keys", h.settingsPlaceholder("keys", "Deploy keys", "Per-repo deploy keys (read-only or read-write SSH keys scoped to one repo) ship in a follow-up sprint."))
	r.Get("/{owner}/{repo}/settings/notifications", h.settingsPlaceholder("notifications", "Notifications", "Repo-scoped notification routing rules ship in a follow-up sprint."))
	r.Get("/{owner}/{repo}/settings/tags", h.settingsPlaceholder("tags", "Tags", "Tag-protection rules ship alongside the wider release tooling."))
}

// settingsGeneral renders the General tab.
func (h *Handlers) settingsGeneral(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	topics, _ := h.rq.ListRepoTopics(r.Context(), h.d.Pool, row.ID)
	sourceRemote, _ := h.repoSourceRemote(r.Context(), row.ID)
	notice := r.URL.Query().Get("notice")
	h.d.Render.RenderPage(w, r, "repo/settings_general", map[string]any{
		"Title":          "General · " + row.Name,
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Owner":          owner.Username,
		"Repo":           row,
		"Topics":         topics,
		"TopicsCSV":      strings.Join(topics, ", "),
		"SourceRemote":   sourceRemote,
		"SettingsActive": "general",
		"Notice":         settingsNoticeMessage(notice),
	})
}

// settingsGeneralUpdate persists Description / HasIssues / HasPulls / Topics.
func (h *Handlers) settingsGeneralUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	description := strings.TrimSpace(r.PostFormValue("description"))
	if len(description) > 350 {
		http.Error(w, "description too long (max 350)", http.StatusBadRequest)
		return
	}
	hasIssues := r.PostFormValue("has_issues") == "on"
	hasPulls := r.PostFormValue("has_pulls") == "on"
	isTemplate := r.PostFormValue("is_template") == "on"
	topicsRaw := splitCommaList(r.PostFormValue("topics"))

	if err := h.rq.UpdateRepoGeneralSettings(r.Context(), h.d.Pool, reposdb.UpdateRepoGeneralSettingsParams{
		ID: row.ID, Description: description, HasIssues: hasIssues, HasPulls: hasPulls, IsTemplate: isTemplate,
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings: general update", "error", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	if err := repos.ReplaceTopics(r.Context(), repos.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, row.ID, topicsRaw); err != nil {
		switch {
		case errors.Is(err, repos.ErrTooManyTopics):
			http.Error(w, "too many topics (max 20)", http.StatusBadRequest)
			return
		case errors.Is(err, repos.ErrInvalidTopic):
			http.Error(w, "invalid topic — use lowercase letters/digits/hyphens, 1–50 chars", http.StatusBadRequest)
			return
		default:
			h.d.Logger.WarnContext(r.Context(), "settings: replace topics", "error", err)
			http.Error(w, "save failed", http.StatusInternalServerError)
			return
		}
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	auditActor, auditMeta := viewer.AuditActor(map[string]any{"action": "general_settings_updated"})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		auditMeta)

	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/general?notice=saved", http.StatusSeeOther)
}

// settingsSourceRemoteUpdate persists the repo's public source remote and
// immediately fetches heads/tags so imported histories and submodule gitlinks
// can resolve without guessing where the objects live.
func (h *Handlers) settingsSourceRemoteUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	rawURL := strings.TrimSpace(r.PostFormValue("source_remote_url"))
	if r.PostFormValue("clear_source_remote") == "1" {
		rawURL = ""
	}
	if rawURL == "" {
		if err := h.rq.DeleteRepoSourceRemote(r.Context(), h.d.Pool, row.ID); err != nil {
			h.d.Logger.WarnContext(r.Context(), "settings: delete source remote", "error", err, "repo_id", row.ID)
			http.Error(w, "save failed", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/general?notice=source-remote-cleared", http.StatusSeeOther)
		return
	}
	remoteURL, err := h.saveRepoSourceRemote(r.Context(), row.ID, rawURL)
	if err != nil {
		if isInvalidSourceRemote(err) {
			http.Error(w, "source remote URL must be a public http(s) git remote without credentials", http.StatusBadRequest)
			return
		}
		h.d.Logger.WarnContext(r.Context(), "settings: save source remote", "error", err, "repo_id", row.ID)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	if err := h.fetchRepoSourceRemote(r.Context(), row, owner.Username, remoteURL); err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings: fetch source remote", "error", err, "repo_id", row.ID, "remote", remoteURL)
		http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/general?notice=source-remote-fetch-failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/general?notice=source-remote-imported", http.StatusSeeOther)
}

// settingsMergeUpdate persists allow_*_merge + default_merge_method.
func (h *Handlers) settingsMergeUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	allowMerge := r.PostFormValue("allow_merge_commit") == "on"
	allowSquash := r.PostFormValue("allow_squash_merge") == "on"
	allowRebase := r.PostFormValue("allow_rebase_merge") == "on"
	if !allowMerge && !allowSquash && !allowRebase {
		http.Error(w, "at least one merge method must be allowed", http.StatusBadRequest)
		return
	}
	def := strings.TrimSpace(r.PostFormValue("default_merge_method"))
	var defMethod reposdb.PrMergeMethod
	switch def {
	case "merge":
		defMethod = reposdb.PrMergeMethodMerge
	case "squash":
		defMethod = reposdb.PrMergeMethodSquash
	case "rebase":
		defMethod = reposdb.PrMergeMethodRebase
	default:
		http.Error(w, "unknown default_merge_method", http.StatusBadRequest)
		return
	}
	// The default must be one of the allowed methods; otherwise PRs
	// merge through a method the admin just disabled.
	if (defMethod == reposdb.PrMergeMethodMerge && !allowMerge) ||
		(defMethod == reposdb.PrMergeMethodSquash && !allowSquash) ||
		(defMethod == reposdb.PrMergeMethodRebase && !allowRebase) {
		http.Error(w, "default merge method must be one of the allowed methods", http.StatusBadRequest)
		return
	}
	if err := h.rq.UpdateRepoMergeSettings(r.Context(), h.d.Pool, reposdb.UpdateRepoMergeSettingsParams{
		ID: row.ID, AllowMergeCommit: allowMerge, AllowSquashMerge: allowSquash,
		AllowRebaseMerge: allowRebase, DefaultMergeMethod: defMethod,
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings: merge update", "error", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	auditActor, auditMeta := viewer.AuditActor(map[string]any{"action": "merge_settings_updated"})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		auditMeta)

	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/general?notice=saved", http.StatusSeeOther)
}

// settingsAccess renders the Access tab — direct collaborators (always)
// and team grants (org-owned repos only). Mutations live in the four
// POST handlers below; this is the read+form view.
func (h *Handlers) settingsAccess(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsCollaborators)
	if !ok {
		return
	}
	pq := policydb.New()
	collabs, _ := pq.ListCollabs(r.Context(), h.d.Pool, row.ID)
	var teamGrants []orgsdb.ListRepoTeamGrantsRow
	var orgTeams []orgsdb.Team
	if row.OwnerOrgID.Valid {
		oq := orgsdb.New()
		teamGrants, _ = oq.ListRepoTeamGrants(r.Context(), h.d.Pool, row.ID)
		orgTeams, _ = oq.ListTeamsForOrg(r.Context(), h.d.Pool, row.OwnerOrgID.Int64)
	}
	notice := r.URL.Query().Get("notice")
	h.d.Render.RenderPage(w, r, "repo/settings_access", map[string]any{
		"Title":          "Access · " + row.Name,
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Owner":          owner.Username,
		"Repo":           row,
		"Collaborators":  collabs,
		"OwnerKindOrg":   row.OwnerOrgID.Valid,
		"TeamGrants":     teamGrants,
		"OrgTeams":       orgTeams,
		"SettingsActive": "access",
		"Notice":         settingsNoticeMessage(notice),
	})
}

// settingsCollabUpsert adds or upgrades a direct collaborator.
func (h *Handlers) settingsCollabUpsert(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsCollaborators)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	role := parseCollabRole(r.PostFormValue("role"))
	if username == "" || role == "" {
		http.Error(w, "username and role required", http.StatusBadRequest)
		return
	}
	target, err := h.uq.GetUserByUsername(r.Context(), h.d.Pool, username)
	if err != nil {
		http.Error(w, "user not found", http.StatusBadRequest)
		return
	}
	// Don't let admins demote an owner to collaborator — for user-owned
	// repos, the owner is implicit admin via policy and shouldn't have
	// a collab row.
	if row.OwnerUserID.Valid && row.OwnerUserID.Int64 == target.ID {
		http.Error(w, "the owner already has admin access", http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := policydb.New().UpsertCollabRole(r.Context(), h.d.Pool, policydb.UpsertCollabRoleParams{
		RepoID: row.ID, UserID: target.ID, Role: role,
		AddedByUserID: pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings: collab upsert", "error", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	policy.InvalidateRepo(r.Context(), row.ID)
	auditActor, auditMeta := viewer.AuditActor(map[string]any{"action": "collaborator_added", "user": username, "role": string(role)})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		auditMeta)
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/access?notice=saved", http.StatusSeeOther)
}

// settingsCollabRemove deletes a direct collaborator row.
func (h *Handlers) settingsCollabRemove(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsCollaborators)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	uid, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("user_id")), 10, 64)
	if err != nil || uid <= 0 {
		http.Error(w, "bad user_id", http.StatusBadRequest)
		return
	}
	if err := policydb.New().RemoveCollab(r.Context(), h.d.Pool, policydb.RemoveCollabParams{
		RepoID: row.ID, UserID: uid,
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings: collab remove", "error", err)
		http.Error(w, "remove failed", http.StatusInternalServerError)
		return
	}
	policy.InvalidateRepo(r.Context(), row.ID)
	viewer := middleware.CurrentUserFromContext(r.Context())
	auditActor, auditMeta := viewer.AuditActor(map[string]any{"action": "collaborator_removed", "user_id": uid})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		auditMeta)
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/access?notice=saved", http.StatusSeeOther)
}

// settingsTeamGrant grants a team a role on this repo (org-owned only).
func (h *Handlers) settingsTeamGrant(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsCollaborators)
	if !ok {
		return
	}
	if !row.OwnerOrgID.Valid {
		http.Error(w, "team grants apply to org-owned repos", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	teamID, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("team_id")), 10, 64)
	if err != nil || teamID <= 0 {
		http.Error(w, "bad team_id", http.StatusBadRequest)
		return
	}
	role := parseTeamRepoRole(r.PostFormValue("role"))
	if role == "" {
		http.Error(w, "role required", http.StatusBadRequest)
		return
	}
	// Defense in depth: ensure the team belongs to this repo's org so a
	// crafted form can't grant a foreign team access.
	team, err := orgsdb.New().GetTeamByID(r.Context(), h.d.Pool, teamID)
	if err != nil || team.OrgID != row.OwnerOrgID.Int64 {
		http.Error(w, "team not found in this org", http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := orgsdb.New().GrantTeamRepoAccess(r.Context(), h.d.Pool, orgsdb.GrantTeamRepoAccessParams{
		TeamID: teamID, RepoID: row.ID, Role: role,
		AddedByUserID: pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings: team grant", "error", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	policy.InvalidateRepo(r.Context(), row.ID)
	auditActor, auditMeta := viewer.AuditActor(map[string]any{"action": "team_grant_added", "team_id": teamID, "role": string(role)})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		auditMeta)
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/access?notice=saved", http.StatusSeeOther)
}

// settingsTeamRevoke revokes a team's grant (org-owned only).
func (h *Handlers) settingsTeamRevoke(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsCollaborators)
	if !ok {
		return
	}
	if !row.OwnerOrgID.Valid {
		http.Error(w, "team grants apply to org-owned repos", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	teamID, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("team_id")), 10, 64)
	if err != nil || teamID <= 0 {
		http.Error(w, "bad team_id", http.StatusBadRequest)
		return
	}
	if err := orgsdb.New().RevokeTeamRepoAccess(r.Context(), h.d.Pool, orgsdb.RevokeTeamRepoAccessParams{
		TeamID: teamID, RepoID: row.ID,
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings: team revoke", "error", err)
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	policy.InvalidateRepo(r.Context(), row.ID)
	viewer := middleware.CurrentUserFromContext(r.Context())
	auditActor, auditMeta := viewer.AuditActor(map[string]any{"action": "team_grant_removed", "team_id": teamID})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		auditMeta)
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/access?notice=saved", http.StatusSeeOther)
}

// parseCollabRole maps form input to the typed role enum, returning ""
// for unknown values (the caller surfaces a 400).
func parseCollabRole(s string) policydb.CollabRole {
	switch strings.TrimSpace(s) {
	case "read":
		return policydb.CollabRoleRead
	case "triage":
		return policydb.CollabRoleTriage
	case "write":
		return policydb.CollabRoleWrite
	case "maintain":
		return policydb.CollabRoleMaintain
	case "admin":
		return policydb.CollabRoleAdmin
	}
	return ""
}

// parseTeamRepoRole is the team_repo_access counterpart.
func parseTeamRepoRole(s string) orgsdb.TeamRepoRole {
	switch strings.TrimSpace(s) {
	case "read":
		return orgsdb.TeamRepoRoleRead
	case "triage":
		return orgsdb.TeamRepoRoleTriage
	case "write":
		return orgsdb.TeamRepoRoleWrite
	case "maintain":
		return orgsdb.TeamRepoRoleMaintain
	case "admin":
		return orgsdb.TeamRepoRoleAdmin
	}
	return ""
}

// settingsPlaceholder renders a shell page explaining a tab is parked
// for a later sprint. Returns the closure so it can be passed to chi
// without the per-call boilerplate.
func (h *Handlers) settingsPlaceholder(active, heading, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
		if !ok {
			return
		}
		h.d.Render.RenderPage(w, r, "repo/settings_placeholder", map[string]any{
			"Title":          heading + " · " + row.Name,
			"CSRFToken":      middleware.CSRFTokenForRequest(r),
			"Owner":          owner.Username,
			"Repo":           row,
			"Heading":        heading,
			"Body":           body,
			"SettingsActive": active,
		})
	}
}

// settingsNoticeMessage maps short query-string codes to user-visible
// strings so the redirect after a POST stays free of long URL params.
func settingsNoticeMessage(code string) string {
	switch code {
	case "saved":
		return "Settings saved."
	case "deleted":
		return "Deleted."
	case "source-remote-cleared":
		return "Source remote cleared."
	case "source-remote-fetch-failed":
		return "Source remote saved, but fetch failed. Check the stored error and try again."
	case "source-remote-imported":
		return "Source remote fetched."
	case "source-remote-save-failed":
		return "Repository was created, but the source remote could not be saved."
	case "":
		return ""
	default:
		return ""
	}
}

func (h *Handlers) repoSourceRemote(ctx context.Context, repoID int64) (reposdb.RepoSourceRemote, bool) {
	sourceRemote, err := h.rq.GetRepoSourceRemote(ctx, h.d.Pool, repoID)
	if err == nil {
		return sourceRemote, true
	}
	if !errors.Is(err, pgx.ErrNoRows) && h.d.Logger != nil {
		h.d.Logger.WarnContext(ctx, "settings: source remote lookup", "error", err, "repo_id", repoID)
	}
	return reposdb.RepoSourceRemote{}, false
}
