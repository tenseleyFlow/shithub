// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	actionsvars "github.com/tenseleyFlow/shithub/internal/actions/variables"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountSettingsActions registers the Actions secrets + variables settings
// routes. Caller wraps with RequireUser; per-route policy gates inside.
func (h *Handlers) MountSettingsActions(r chi.Router) {
	r.Get("/{owner}/{repo}/settings/actions", h.settingsActionsPolicy)
	r.Post("/{owner}/{repo}/settings/actions", h.settingsActionsPolicyUpdate)
	r.Get("/{owner}/{repo}/settings/secrets/actions", h.settingsActionsSecrets)
	r.Post("/{owner}/{repo}/settings/secrets/actions", h.settingsActionsSecretSet)
	r.Post("/{owner}/{repo}/settings/secrets/actions/{name}/delete", h.settingsActionsSecretDelete)
	r.Get("/{owner}/{repo}/settings/variables/actions", h.settingsActionsVariables)
	r.Post("/{owner}/{repo}/settings/variables/actions", h.settingsActionsVariableSet)
	r.Post("/{owner}/{repo}/settings/variables/actions/{name}/delete", h.settingsActionsVariableDelete)
}

type repoActionsPolicyForm struct {
	ActionsEnabled              string
	RequirePRApproval           string
	MaxRepoQueuedRuns           string
	MaxRepoConcurrentJobs       string
	MaxOwnerConcurrentJobs      string
	ActorTriggerLimitPerHour    string
	EffectiveActionsEnabled     bool
	EffectiveRequirePRApproval  bool
	EffectiveMaxRepoQueuedRuns  int32
	EffectiveMaxRepoConcurrent  int32
	EffectiveMaxOwnerConcurrent int32
	EffectiveActorHourlyLimit   int32
}

func (h *Handlers) settingsActionsPolicy(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	h.renderRepoActionsPolicySettings(w, r, row, owner.Username, "", settingsNoticeMessage(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsActionsPolicyUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	form, err := repoActionsPolicyFormFromRequest(r)
	if err != nil {
		h.renderRepoActionsPolicySettings(w, r, row, owner.Username, err.Error(), "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := actionsdb.New().UpsertActionsRepoPolicy(r.Context(), h.d.Pool, actionsdb.UpsertActionsRepoPolicyParams{
		RepoID:                   row.ID,
		ActionsEnabled:           actionsdb.ActionsPolicyState(form.ActionsEnabled),
		RequirePrApproval:        nullableBoolSetting(form.RequirePRApproval),
		MaxRepoQueuedRuns:        nullableInt4Setting(form.MaxRepoQueuedRuns),
		MaxRepoConcurrentJobs:    nullableInt4Setting(form.MaxRepoConcurrentJobs),
		MaxOwnerConcurrentJobs:   nullableInt4Setting(form.MaxOwnerConcurrentJobs),
		ActorTriggerLimitPerHour: nullableInt4Setting(form.ActorTriggerLimitPerHour),
		UpdatedByUserID:          pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "actions policy: update repo", "repo_id", row.ID, "error", err)
		h.renderRepoActionsPolicySettings(w, r, row, owner.Username, "Could not save Actions policy.", "")
		return
	}
	actor, meta := viewer.AuditActor(map[string]any{
		"actions_enabled":              form.ActionsEnabled,
		"require_pr_approval":          form.RequirePRApproval,
		"max_repo_queued_runs":         form.MaxRepoQueuedRuns,
		"max_repo_concurrent_jobs":     form.MaxRepoConcurrentJobs,
		"max_owner_concurrent_jobs":    form.MaxOwnerConcurrentJobs,
		"actor_trigger_limit_per_hour": form.ActorTriggerLimitPerHour,
	})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, audit.ActionActionsPolicyUpdated, audit.TargetRepo, row.ID, meta)
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/actions?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) renderRepoActionsPolicySettings(w http.ResponseWriter, r *http.Request, row reposdb.Repo, owner, errMsg, notice string) {
	form, loadErr := h.loadRepoActionsPolicyForm(r, row.ID)
	if loadErr != nil {
		h.d.Logger.WarnContext(r.Context(), "actions policy: load repo settings", "repo_id", row.ID, "error", loadErr)
		errMsg = "Could not load Actions policy."
	}
	data := map[string]any{
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Title":          "Actions settings · " + row.Name,
		"Owner":          owner,
		"Repo":           row,
		"SettingsActive": "actions",
		"Error":          errMsg,
		"Notice":         notice,
		"Policy":         form,
	}
	h.d.Render.RenderPage(w, r, "repo/settings_actions", data)
}

func (h *Handlers) loadRepoActionsPolicyForm(r *http.Request, repoID int64) (repoActionsPolicyForm, error) {
	q := actionsdb.New()
	eff, err := q.GetEffectiveActionsPolicyForRepo(r.Context(), h.d.Pool, repoID)
	if err != nil {
		return repoActionsPolicyForm{}, err
	}
	form := repoActionsPolicyForm{
		ActionsEnabled:              "inherit",
		RequirePRApproval:           "inherit",
		EffectiveActionsEnabled:     eff.ActionsEnabled,
		EffectiveRequirePRApproval:  eff.RequirePrApproval,
		EffectiveMaxRepoQueuedRuns:  eff.MaxRepoQueuedRuns,
		EffectiveMaxRepoConcurrent:  eff.MaxRepoConcurrentJobs,
		EffectiveMaxOwnerConcurrent: eff.MaxOwnerConcurrentJobs,
		EffectiveActorHourlyLimit:   eff.ActorTriggerLimitPerHour,
	}
	row, err := q.GetActionsRepoPolicy(r.Context(), h.d.Pool, repoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return form, nil
		}
		return repoActionsPolicyForm{}, err
	}
	form.ActionsEnabled = string(row.ActionsEnabled)
	form.RequirePRApproval = nullableBoolSettingString(row.RequirePrApproval)
	form.MaxRepoQueuedRuns = nullableInt4SettingString(row.MaxRepoQueuedRuns)
	form.MaxRepoConcurrentJobs = nullableInt4SettingString(row.MaxRepoConcurrentJobs)
	form.MaxOwnerConcurrentJobs = nullableInt4SettingString(row.MaxOwnerConcurrentJobs)
	form.ActorTriggerLimitPerHour = nullableInt4SettingString(row.ActorTriggerLimitPerHour)
	return form, nil
}

func repoActionsPolicyFormFromRequest(r *http.Request) (repoActionsPolicyForm, error) {
	form := repoActionsPolicyForm{
		ActionsEnabled:           strings.TrimSpace(r.PostFormValue("actions_enabled")),
		RequirePRApproval:        strings.TrimSpace(r.PostFormValue("require_pr_approval")),
		MaxRepoQueuedRuns:        strings.TrimSpace(r.PostFormValue("max_repo_queued_runs")),
		MaxRepoConcurrentJobs:    strings.TrimSpace(r.PostFormValue("max_repo_concurrent_jobs")),
		MaxOwnerConcurrentJobs:   strings.TrimSpace(r.PostFormValue("max_owner_concurrent_jobs")),
		ActorTriggerLimitPerHour: strings.TrimSpace(r.PostFormValue("actor_trigger_limit_per_hour")),
	}
	switch form.ActionsEnabled {
	case "inherit", "enabled", "disabled":
	default:
		return repoActionsPolicyForm{}, errors.New("Invalid Actions enablement setting.")
	}
	switch form.RequirePRApproval {
	case "inherit", "true", "false":
	default:
		return repoActionsPolicyForm{}, errors.New("Invalid pull request approval setting.")
	}
	for label, value := range map[string]string{
		"queued run cap":           form.MaxRepoQueuedRuns,
		"repository concurrency":   form.MaxRepoConcurrentJobs,
		"owner concurrency":        form.MaxOwnerConcurrentJobs,
		"actor hourly trigger cap": form.ActorTriggerLimitPerHour,
	} {
		if err := validateOptionalNonnegativeInt(value); err != nil {
			return repoActionsPolicyForm{}, fmt.Errorf("Invalid %s.", label)
		}
	}
	return form, nil
}

func validateOptionalNonnegativeInt(v string) error {
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		return errors.New("invalid integer")
	}
	return nil
}

func nullableBoolSetting(v string) pgtype.Bool {
	switch v {
	case "true":
		return pgtype.Bool{Bool: true, Valid: true}
	case "false":
		return pgtype.Bool{Bool: false, Valid: true}
	default:
		return pgtype.Bool{}
	}
}

func nullableBoolSettingString(v pgtype.Bool) string {
	if !v.Valid {
		return "inherit"
	}
	if v.Bool {
		return "true"
	}
	return "false"
}

func nullableInt4Setting(v string) pgtype.Int4 {
	if v == "" {
		return pgtype.Int4{}
	}
	n, _ := strconv.ParseInt(v, 10, 32)
	return pgtype.Int4{Int32: int32(n), Valid: true}
}

func nullableInt4SettingString(v pgtype.Int4) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatInt(int64(v.Int32), 10)
}

func (h *Handlers) settingsActionsSecrets(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	h.renderRepoActionsSettings(w, r, row, owner.Username, "secrets", "", settingsNoticeMessage(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsActionsSecretSet(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	if h.d.SecretBox == nil {
		http.Error(w, "actions secret key not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	name := strings.TrimSpace(r.PostFormValue("name"))
	value := []byte(r.PostFormValue("value"))
	err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.Set(r.Context(), secrets.RepoScope(row.ID), name, value, viewer.ID)
	if err != nil {
		h.renderRepoActionsSettings(w, r, row, owner.Username, "secrets", friendlyActionsSecretError(err), "")
		return
	}
	h.recordRepoActionsAudit(r, viewer, audit.ActionActionsSecretSet, row.ID, name)
	http.Redirect(w, r, repoActionsSettingsPath(owner.Username, row.Name, "secrets")+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsSecretDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.Delete(r.Context(), secrets.RepoScope(row.ID), name)
	if err != nil {
		if errors.Is(err, secrets.ErrInvalidName) {
			http.Error(w, "bad secret name", http.StatusBadRequest)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	h.recordRepoActionsAudit(r, viewer, audit.ActionActionsSecretDeleted, row.ID, name)
	http.Redirect(w, r, repoActionsSettingsPath(owner.Username, row.Name, "secrets")+"?notice=deleted", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsVariables(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	h.renderRepoActionsSettings(w, r, row, owner.Username, "variables", "", settingsNoticeMessage(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsActionsVariableSet(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	name := strings.TrimSpace(r.PostFormValue("name"))
	value := r.PostFormValue("value")
	err := actionsvars.Deps{Pool: h.d.Pool}.Set(r.Context(), actionsvars.RepoScope(row.ID), name, value, viewer.ID)
	if err != nil {
		h.renderRepoActionsSettings(w, r, row, owner.Username, "variables", friendlyActionsVariableError(err), "")
		return
	}
	h.recordRepoActionsAudit(r, viewer, audit.ActionActionsVariableSet, row.ID, name)
	http.Redirect(w, r, repoActionsSettingsPath(owner.Username, row.Name, "variables")+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsVariableDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	err := actionsvars.Deps{Pool: h.d.Pool}.Delete(r.Context(), actionsvars.RepoScope(row.ID), name)
	if err != nil {
		if errors.Is(err, actionsvars.ErrInvalidName) {
			http.Error(w, "bad variable name", http.StatusBadRequest)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	h.recordRepoActionsAudit(r, viewer, audit.ActionActionsVariableDeleted, row.ID, name)
	http.Redirect(w, r, repoActionsSettingsPath(owner.Username, row.Name, "variables")+"?notice=deleted", http.StatusSeeOther)
}

func (h *Handlers) renderRepoActionsSettings(w http.ResponseWriter, r *http.Request, row reposdb.Repo, owner, kind, errMsg, notice string) {
	data := map[string]any{
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Owner":     owner,
		"Repo":      row,
		"Kind":      kind,
		"Error":     errMsg,
		"Notice":    notice,
	}
	switch kind {
	case "secrets":
		items, err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.List(r.Context(), secrets.RepoScope(row.ID))
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "actions secrets: list repo", "repo_id", row.ID, "error", err)
			items = nil
		}
		data["Title"] = "Actions secrets · " + row.Name
		data["Heading"] = "Actions secrets"
		data["IsSecrets"] = true
		data["Secrets"] = items
		data["SecretDisabled"] = h.d.SecretBox == nil
		data["SettingsActive"] = "actions-secrets"
		data["FormAction"] = repoActionsSettingsPath(owner, row.Name, "secrets")
	case "variables":
		items, err := actionsvars.Deps{Pool: h.d.Pool}.List(r.Context(), actionsvars.RepoScope(row.ID))
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "actions variables: list repo", "repo_id", row.ID, "error", err)
			items = nil
		}
		data["Title"] = "Actions variables · " + row.Name
		data["Heading"] = "Actions variables"
		data["Variables"] = items
		data["SettingsActive"] = "actions-variables"
		data["FormAction"] = repoActionsSettingsPath(owner, row.Name, "variables")
	}
	h.d.Render.RenderPage(w, r, "repo/settings_secrets", data)
}

func repoActionsSettingsPath(owner, repoName, kind string) string {
	return "/" + owner + "/" + repoName + "/settings/" + kind + "/actions"
}

func friendlyActionsSecretError(err error) string {
	switch {
	case errors.Is(err, secrets.ErrInvalidName):
		return "Name must start with a letter or underscore and contain only letters, numbers, and underscores."
	case errors.Is(err, secrets.ErrEmptyValue):
		return "Secret value is required."
	case errors.Is(err, secrets.ErrInvalidScope):
		return "Invalid secret scope."
	default:
		return "Could not save secret."
	}
}

func friendlyActionsVariableError(err error) string {
	switch {
	case errors.Is(err, actionsvars.ErrInvalidName):
		return "Name must start with a letter or underscore and contain only letters, numbers, and underscores."
	case errors.Is(err, actionsvars.ErrValueTooLong):
		return "Variable value must be 4096 characters or fewer."
	case errors.Is(err, actionsvars.ErrInvalidScope):
		return "Invalid variable scope."
	default:
		return "Could not save variable."
	}
}

func (h *Handlers) recordRepoActionsAudit(r *http.Request, viewer middleware.CurrentUser, action audit.Action, repoID int64, name string) {
	actor, meta := viewer.AuditActor(map[string]any{"name": name})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, action, audit.TargetRepo, repoID, meta)
}
