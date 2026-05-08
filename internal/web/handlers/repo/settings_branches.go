// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountSettingsBranches registers the branch-protection + default-
// branch settings routes. Caller wraps with RequireUser.
func (h *Handlers) MountSettingsBranches(r chi.Router) {
	r.Get("/{owner}/{repo}/settings/branches", h.settingsBranches)
	r.Post("/{owner}/{repo}/settings/branches", h.settingsBranchesUpsert)
	r.Post("/{owner}/{repo}/settings/branches/{id}/delete", h.settingsBranchesDelete)
	r.Post("/{owner}/{repo}/settings/default-branch", h.settingsDefaultBranch)
}

// settingsBranches lists existing protection rules + a form to create
// a new one. Gated by repo:settings:branches.
func (h *Handlers) settingsBranches(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsBranches)
	if !ok {
		return
	}
	rules, err := h.rq.ListBranchProtectionRules(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	gitDir, _ := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	refs, _ := repogit.ListRefs(r.Context(), gitDir)

	h.d.Render.RenderPage(w, r, "repo/settings_branches", map[string]any{
		"Title":     "Branch protection · " + row.Name,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Owner":     owner.Username,
		"Repo":      row,
		"Rules":     rules,
		"Branches":  refs.Branches,
	})
}

// settingsBranchesUpsert creates a new rule (no `id` param) or updates
// an existing one (`id` param set). Form fields: pattern,
// prevent_force_push, prevent_deletion, require_pr_for_push,
// allowed_pusher_usernames (comma-separated).
func (h *Handlers) settingsBranchesUpsert(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsBranches)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	pattern := strings.TrimSpace(r.PostFormValue("pattern"))
	if pattern == "" || len(pattern) > 200 {
		http.Error(w, "pattern length must be 1–200", http.StatusBadRequest)
		return
	}
	preventForcePush := r.PostFormValue("prevent_force_push") == "on"
	preventDeletion := r.PostFormValue("prevent_deletion") == "on"
	requirePR := r.PostFormValue("require_pr_for_push") == "on"

	allowed, err := resolveUsernameList(r, h, r.PostFormValue("allowed_pushers"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	viewer := middleware.CurrentUserFromContext(r.Context())

	idStr := r.PostFormValue("id")
	if idStr == "" {
		// Create.
		newID, err := h.rq.UpsertBranchProtectionRule(r.Context(), h.d.Pool, reposdb.UpsertBranchProtectionRuleParams{
			RepoID:                row.ID,
			Pattern:               pattern,
			PreventForcePush:      preventForcePush,
			PreventDeletion:       preventDeletion,
			RequirePrForPush:      requirePR,
			AllowedPusherUserIds:  allowed,
			CreatedByUserID:       pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
		})
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "branch-protection: insert", "error", err)
			http.Error(w, "failed to create rule", http.StatusInternalServerError)
			return
		}
		_ = h.d.Audit.Record(r.Context(), h.d.Pool, viewer.ID,
			audit.ActionRepoCreated, audit.TargetRepo, row.ID,
			map[string]any{"branch_protection_rule_id": newID, "pattern": pattern, "action": "create"})
	} else {
		// Update.
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		// Defense in depth: confirm the rule belongs to this repo.
		existing, err := h.rq.GetBranchProtectionRule(r.Context(), h.d.Pool, id)
		if err != nil || existing.RepoID != row.ID {
			http.Error(w, "rule not found", http.StatusNotFound)
			return
		}
		if err := h.rq.UpdateBranchProtectionRule(r.Context(), h.d.Pool, reposdb.UpdateBranchProtectionRuleParams{
			ID:                   id,
			Pattern:              pattern,
			PreventForcePush:     preventForcePush,
			PreventDeletion:      preventDeletion,
			RequirePrForPush:     requirePR,
			AllowedPusherUserIds: allowed,
		}); err != nil {
			http.Error(w, "failed to update rule", http.StatusInternalServerError)
			return
		}
		_ = h.d.Audit.Record(r.Context(), h.d.Pool, viewer.ID,
			audit.ActionRepoCreated, audit.TargetRepo, row.ID,
			map[string]any{"branch_protection_rule_id": id, "pattern": pattern, "action": "update"})
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/branches?notice=saved", http.StatusSeeOther)
}

// settingsBranchesDelete removes a rule.
func (h *Handlers) settingsBranchesDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsBranches)
	if !ok {
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	existing, err := h.rq.GetBranchProtectionRule(r.Context(), h.d.Pool, id)
	if err != nil || existing.RepoID != row.ID {
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}
	if err := h.rq.DeleteBranchProtectionRule(r.Context(), h.d.Pool, id); err != nil {
		http.Error(w, "failed to delete rule", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, viewer.ID,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		map[string]any{"branch_protection_rule_id": id, "pattern": existing.Pattern, "action": "delete"})

	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/branches?notice=deleted", http.StatusSeeOther)
}

// settingsDefaultBranch swaps the repo's default branch. Validates
// the target exists, updates the DB row, and updates HEAD on disk via
// `git symbolic-ref`.
func (h *Handlers) settingsDefaultBranch(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsBranches)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	newDefault := strings.TrimSpace(r.PostFormValue("default_branch"))
	if newDefault == "" {
		http.Error(w, "default_branch required", http.StatusBadRequest)
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	refs, err := repogit.ListRefs(r.Context(), gitDir)
	if err != nil {
		http.Error(w, "ref lookup failed", http.StatusInternalServerError)
		return
	}
	exists := false
	for _, b := range refs.Branches {
		if b.Name == newDefault {
			exists = true
			break
		}
	}
	if !exists {
		http.Error(w, "branch not found", http.StatusBadRequest)
		return
	}

	if err := h.rq.UpdateRepoDefaultBranch(r.Context(), h.d.Pool, reposdb.UpdateRepoDefaultBranchParams{
		ID: row.ID, DefaultBranch: newDefault,
	}); err != nil {
		http.Error(w, "DB update failed", http.StatusInternalServerError)
		return
	}
	if err := repogit.SetSymbolicRef(r.Context(), gitDir, "HEAD", "refs/heads/"+newDefault); err != nil {
		// DB updated but symbolic-ref failed — log and surface, but don't roll back DB
		// since the user-visible truth is the DB row (their UI shows it; new clones
		// follow it). Operator can re-run by setting it again.
		h.d.Logger.WarnContext(r.Context(), "default-branch: symbolic-ref", "error", err)
	}

	viewer := middleware.CurrentUserFromContext(r.Context())
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, viewer.ID,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		map[string]any{"action": "default_branch_changed", "from": row.DefaultBranch, "to": newDefault})

	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/branches?notice=default-changed", http.StatusSeeOther)
}

// resolveUsernameList parses a comma-separated username list and
// resolves each to a user_id. Empty input returns an empty slice
// (no allowed-pushers restriction). Unknown usernames produce an
// error so the admin sees the typo before the rule lands.
func resolveUsernameList(r *http.Request, h *Handlers, raw string) ([]int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []int64{}, nil
	}
	uq := usersdb.New()
	parts := strings.Split(raw, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		name := strings.ToLower(strings.TrimSpace(p))
		if name == "" {
			continue
		}
		u, err := uq.GetUserByUsername(r.Context(), h.d.Pool, name)
		if err != nil {
			return nil, errors.New("unknown username: " + name)
		}
		out = append(out, u.ID)
	}
	return out, nil
}
