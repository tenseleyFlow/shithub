// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
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
		"Title":          "Branch protection · " + row.Name,
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Owner":          owner.Username,
		"Repo":           row,
		"Rules":          rules,
		"Branches":       refs.Branches,
		"Notice":         settingsBranchesNoticeMessage(r.URL.Query().Get("notice")),
		"SettingsActive": "branches",
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

	requiredReviews, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("required_review_count")))
	if requiredReviews < 0 {
		requiredReviews = 0
	}
	dismissStale := r.PostFormValue("dismiss_stale_reviews_on_push") == "on"

	// S24 required-status-check names: comma-separated input. Empty
	// list means no required checks. Names are matched verbatim
	// against `check_runs.name` at gate time.
	requiredChecks := splitCommaList(r.PostFormValue("required_status_check_names"))
	dismissStaleChecks := r.PostFormValue("dismiss_stale_status_checks_on_push") == "on"

	noticeCode, err := h.branchProtectionEntitlementNotice(r.Context(), row, requiredReviews, dismissStale, requiredChecks, dismissStaleChecks)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if noticeCode != "" {
		http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/branches?notice="+noticeCode, http.StatusSeeOther)
		return
	}

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
			RepoID:               row.ID,
			Pattern:              pattern,
			PreventForcePush:     preventForcePush,
			PreventDeletion:      preventDeletion,
			RequirePrForPush:     requirePR,
			AllowedPusherUserIds: allowed,
			CreatedByUserID:      pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
		})
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "branch-protection: insert", "error", err)
			http.Error(w, "failed to create rule", http.StatusInternalServerError)
			return
		}
		if err := h.rq.UpdateBranchProtectionReviewSettings(r.Context(), h.d.Pool, reposdb.UpdateBranchProtectionReviewSettingsParams{
			ID:                        newID,
			RequiredReviewCount:       int32(requiredReviews),
			DismissStaleReviewsOnPush: dismissStale,
			RequireCodeOwnerReview:    false,
		}); err != nil {
			h.d.Logger.WarnContext(r.Context(), "branch-protection: review settings", "error", err)
		}
		if err := h.rq.UpdateBranchProtectionCheckSettings(r.Context(), h.d.Pool, reposdb.UpdateBranchProtectionCheckSettingsParams{
			ID:                             newID,
			StatusChecksRequired:           requiredChecks,
			DismissStaleStatusChecksOnPush: dismissStaleChecks,
		}); err != nil {
			h.d.Logger.WarnContext(r.Context(), "branch-protection: check settings", "error", err)
		}
		auditActor, auditMeta := viewer.AuditActor(map[string]any{"branch_protection_rule_id": newID, "pattern": pattern, "action": "create"})
		_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor,
			audit.ActionRepoCreated, audit.TargetRepo, row.ID,
			auditMeta)
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
		if err := h.rq.UpdateBranchProtectionReviewSettings(r.Context(), h.d.Pool, reposdb.UpdateBranchProtectionReviewSettingsParams{
			ID:                        id,
			RequiredReviewCount:       int32(requiredReviews),
			DismissStaleReviewsOnPush: dismissStale,
			RequireCodeOwnerReview:    false,
		}); err != nil {
			h.d.Logger.WarnContext(r.Context(), "branch-protection: review settings", "error", err)
		}
		if err := h.rq.UpdateBranchProtectionCheckSettings(r.Context(), h.d.Pool, reposdb.UpdateBranchProtectionCheckSettingsParams{
			ID:                             id,
			StatusChecksRequired:           requiredChecks,
			DismissStaleStatusChecksOnPush: dismissStaleChecks,
		}); err != nil {
			h.d.Logger.WarnContext(r.Context(), "branch-protection: check settings", "error", err)
		}
		auditActor, auditMeta := viewer.AuditActor(map[string]any{"branch_protection_rule_id": id, "pattern": pattern, "action": "update"})
		_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor,
			audit.ActionRepoCreated, audit.TargetRepo, row.ID,
			auditMeta)
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/branches?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) branchProtectionEntitlementNotice(ctx context.Context, row reposdb.Repo, requiredReviews int, dismissStale bool, requiredChecks []string, dismissStaleChecks bool) (string, error) {
	if !row.OwnerOrgID.Valid || row.Visibility != reposdb.RepoVisibilityPrivate {
		return "", nil
	}
	if requiredReviews > 0 || dismissStale {
		decision, err := entitlements.CheckOrgFeature(ctx, entitlements.Deps{Pool: h.d.Pool}, row.OwnerOrgID.Int64, entitlements.FeatureOrgRequiredReviewers)
		if err != nil {
			return "", err
		}
		if !decision.Allowed {
			return branchProtectionNoticeCode(decision, true), nil
		}
	}
	if len(requiredChecks) > 0 || dismissStaleChecks {
		decision, err := entitlements.CheckOrgFeature(ctx, entitlements.Deps{Pool: h.d.Pool}, row.OwnerOrgID.Int64, entitlements.FeatureOrgAdvancedBranchProtection)
		if err != nil {
			return "", err
		}
		if !decision.Allowed {
			return branchProtectionNoticeCode(decision, false), nil
		}
	}
	return "", nil
}

func branchProtectionNoticeCode(decision entitlements.Decision, requiredReviewers bool) string {
	if requiredReviewers {
		if decision.Reason == entitlements.ReasonBillingActionNeeded {
			return "required-reviewers-billing"
		}
		return "required-reviewers-upgrade"
	}
	if decision.Reason == entitlements.ReasonBillingActionNeeded {
		return "branch-protection-billing"
	}
	return "branch-protection-upgrade"
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
	auditActor, auditMeta := viewer.AuditActor(map[string]any{"branch_protection_rule_id": id, "pattern": existing.Pattern, "action": "delete"})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		auditMeta)

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
	auditActor, auditMeta := viewer.AuditActor(map[string]any{"action": "default_branch_changed", "from": row.DefaultBranch, "to": newDefault})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		auditMeta)

	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/branches?notice=default-changed", http.StatusSeeOther)
}

func settingsBranchesNoticeMessage(code string) string {
	if msg := settingsNoticeMessage(code); msg != "" {
		return msg
	}
	switch code {
	case "default-changed":
		return "Default branch updated."
	case "branch-protection-upgrade":
		return "Advanced branch protection on private organization repositories requires Team billing."
	case "branch-protection-billing":
		return "Advanced branch protection is read-only until Team billing is brought back into good standing."
	case "required-reviewers-upgrade":
		return "Required reviewers on private organization repositories require Team billing."
	case "required-reviewers-billing":
		return "Required reviewers are read-only until Team billing is brought back into good standing."
	default:
		return ""
	}
}

// splitCommaList parses a comma-separated string into a deduplicated,
// trimmed slice. Empty entries drop. Used by S24's required-check
// name input on the protection-rule form.
func splitCommaList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}
	}
	seen := map[string]struct{}{}
	out := []string{}
	for _, p := range strings.Split(raw, ",") {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
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
