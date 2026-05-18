// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountSettingsActions registers the Actions secrets + variables settings
// routes. Caller wraps with RequireUser; per-route policy gates inside.
func (h *Handlers) MountSettingsActions(r chi.Router) {
	r.Get("/{owner}/{repo}/settings/actions", h.settingsActionsPolicy)
	r.Post("/{owner}/{repo}/settings/actions", h.settingsActionsPolicyUpdate)
	r.Get("/{owner}/{repo}/settings/environments", h.settingsActionsEnvironments)
	r.Post("/{owner}/{repo}/settings/environments", h.settingsActionsEnvironmentCreate)
	r.Get("/{owner}/{repo}/settings/environments/{environment}", h.settingsActionsEnvironmentDetail)
	r.Post("/{owner}/{repo}/settings/environments/{environment}", h.settingsActionsEnvironmentUpdate)
	r.Post("/{owner}/{repo}/settings/environments/{environment}/delete", h.settingsActionsEnvironmentDelete)
	r.Post("/{owner}/{repo}/settings/environments/{environment}/secrets", h.settingsActionsEnvironmentSecretSet)
	r.Post("/{owner}/{repo}/settings/environments/{environment}/secrets/{name}/delete", h.settingsActionsEnvironmentSecretDelete)
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

type repoActionsEnvironmentView struct {
	Name                   string
	Href                   string
	DeploymentBranchPolicy string
	RequiredReviewers      bool
	PreventSelfReview      bool
	WaitTimerMinutes       int32
	BranchPatterns         []string
	Secrets                []secrets.Meta
	SecretCount            int
}

type repoActionsEnvironmentForm struct {
	Name                   string
	DeploymentBranchPolicy string
	RequiredReviewers      bool
	PreventSelfReview      bool
	WaitTimerMinutes       int32
	BranchPatterns         []string
	BranchPatternsText     string
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
		return repoActionsPolicyForm{}, errors.New("invalid Actions enablement setting")
	}
	switch form.RequirePRApproval {
	case "inherit", "true", "false":
	default:
		return repoActionsPolicyForm{}, errors.New("invalid pull request approval setting")
	}
	for label, value := range map[string]string{
		"queued run cap":           form.MaxRepoQueuedRuns,
		"repository concurrency":   form.MaxRepoConcurrentJobs,
		"owner concurrency":        form.MaxOwnerConcurrentJobs,
		"actor hourly trigger cap": form.ActorTriggerLimitPerHour,
	} {
		if err := validateOptionalNonnegativeInt(value); err != nil {
			return repoActionsPolicyForm{}, fmt.Errorf("invalid %s", label)
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

func (h *Handlers) settingsActionsEnvironments(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	h.renderRepoActionsEnvironments(w, r, row, owner.Username, "", "", settingsNoticeMessage(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsActionsEnvironmentDetail(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	h.renderRepoActionsEnvironments(w, r, row, owner.Username, chi.URLParam(r, "environment"), "", settingsNoticeMessage(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsActionsEnvironmentCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureAdvancedBranchProtection, "Environment protection rules") {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	form, err := repoActionsEnvironmentFormFromRequest(r, "")
	if err != nil {
		h.renderRepoActionsEnvironments(w, r, row, owner.Username, "", err.Error(), "")
		return
	}
	if _, err := h.saveRepoActionsEnvironment(r, row.ID, form); err != nil {
		h.d.Logger.WarnContext(r.Context(), "actions environments: create", "repo_id", row.ID, "error", err)
		h.renderRepoActionsEnvironments(w, r, row, owner.Username, "", "Could not save environment.", "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor, meta := viewer.AuditActor(map[string]any{"environment": form.Name, "action": "create"})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, audit.ActionActionsPolicyUpdated, audit.TargetRepo, row.ID, meta)
	http.Redirect(w, r, repoActionsEnvironmentPath(owner.Username, row.Name, form.Name)+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsEnvironmentUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	envName := chi.URLParam(r, "environment")
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureAdvancedBranchProtection, "Environment protection rules") {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	form, err := repoActionsEnvironmentFormFromRequest(r, envName)
	if err != nil {
		h.renderRepoActionsEnvironments(w, r, row, owner.Username, envName, err.Error(), "")
		return
	}
	if _, err := h.saveRepoActionsEnvironment(r, row.ID, form); err != nil {
		h.d.Logger.WarnContext(r.Context(), "actions environments: update", "repo_id", row.ID, "environment", envName, "error", err)
		h.renderRepoActionsEnvironments(w, r, row, owner.Username, envName, "Could not save environment.", "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor, meta := viewer.AuditActor(map[string]any{"environment": form.Name, "action": "update"})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, audit.ActionActionsPolicyUpdated, audit.TargetRepo, row.ID, meta)
	http.Redirect(w, r, repoActionsEnvironmentPath(owner.Username, row.Name, form.Name)+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsEnvironmentDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	envName := chi.URLParam(r, "environment")
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureAdvancedBranchProtection, "Environment protection rules") {
		return
	}
	if err := actionsdb.New().DeleteRepoEnvironment(r.Context(), h.d.Pool, actionsdb.DeleteRepoEnvironmentParams{
		RepoID: row.ID,
		Name:   envName,
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "actions environments: delete", "repo_id", row.ID, "environment", envName, "error", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor, meta := viewer.AuditActor(map[string]any{"environment": envName, "action": "delete"})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, audit.ActionActionsPolicyUpdated, audit.TargetRepo, row.ID, meta)
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/environments?notice=deleted", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsEnvironmentSecretSet(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	envName := chi.URLParam(r, "environment")
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureActionsOrgSecrets, "Environment Actions secrets") {
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
	env, err := actionsdb.New().GetRepoEnvironmentByName(r.Context(), h.d.Pool, actionsdb.GetRepoEnvironmentByNameParams{
		RepoID: row.ID,
		Name:   envName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "actions environments: secret env lookup", "repo_id", row.ID, "environment", envName, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	name := strings.TrimSpace(r.PostFormValue("name"))
	value := []byte(r.PostFormValue("value"))
	err = secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.Set(r.Context(), secrets.EnvironmentScope(env.ID), name, value, viewer.ID)
	if err != nil {
		h.renderRepoActionsEnvironments(w, r, row, owner.Username, envName, friendlyActionsSecretError(err), "")
		return
	}
	actor, meta := viewer.AuditActor(map[string]any{"environment": envName, "name": name})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, audit.ActionActionsSecretSet, audit.TargetRepo, row.ID, meta)
	http.Redirect(w, r, repoActionsEnvironmentPath(owner.Username, row.Name, envName)+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsEnvironmentSecretDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	envName := chi.URLParam(r, "environment")
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureActionsOrgSecrets, "Environment Actions secrets") {
		return
	}
	env, err := actionsdb.New().GetRepoEnvironmentByName(r.Context(), h.d.Pool, actionsdb.GetRepoEnvironmentByNameParams{
		RepoID: row.ID,
		Name:   envName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "actions environments: delete secret env lookup", "repo_id", row.ID, "environment", envName, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	name := chi.URLParam(r, "name")
	err = secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.Delete(r.Context(), secrets.EnvironmentScope(env.ID), name)
	if err != nil {
		if errors.Is(err, secrets.ErrInvalidName) {
			http.Error(w, "bad secret name", http.StatusBadRequest)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor, meta := viewer.AuditActor(map[string]any{"environment": envName, "name": name})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, audit.ActionActionsSecretDeleted, audit.TargetRepo, row.ID, meta)
	http.Redirect(w, r, repoActionsEnvironmentPath(owner.Username, row.Name, envName)+"?notice=deleted", http.StatusSeeOther)
}

func (h *Handlers) renderRepoActionsEnvironments(w http.ResponseWriter, r *http.Request, row reposdb.Repo, owner, selectedName, errMsg, notice string) {
	envs, selected, loadErr := h.loadRepoActionsEnvironmentViews(r, row.ID, owner, row.Name, selectedName)
	if loadErr != nil {
		h.d.Logger.WarnContext(r.Context(), "actions environments: load", "repo_id", row.ID, "environment", selectedName, "error", loadErr)
		if errors.Is(loadErr, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		errMsg = "Could not load environments."
	}
	data := map[string]any{
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Title":          "Actions environments · " + row.Name,
		"Owner":          owner,
		"Repo":           row,
		"SettingsActive": "actions-environments",
		"Error":          errMsg,
		"Notice":         notice,
		"Environments":   envs,
		"Selected":       selected,
		"SecretDisabled": h.d.SecretBox == nil,
		"CreateAction":   "/" + owner + "/" + row.Name + "/settings/environments",
	}
	h.d.Render.RenderPage(w, r, "repo/settings_environments", data)
}

func (h *Handlers) loadRepoActionsEnvironmentViews(r *http.Request, repoID int64, owner, repoName, selectedName string) ([]repoActionsEnvironmentView, *repoActionsEnvironmentView, error) {
	q := actionsdb.New()
	rows, err := q.ListRepoEnvironments(r.Context(), h.d.Pool, repoID)
	if err != nil {
		return nil, nil, err
	}
	views := make([]repoActionsEnvironmentView, 0, len(rows))
	var selected *repoActionsEnvironmentView
	for _, env := range rows {
		view, err := h.repoActionsEnvironmentView(r, env.ID, owner, repoName, env.Name, string(env.DeploymentBranchPolicy), env.RequiredReviewersEnabled, env.PreventSelfReview, env.WaitTimerMinutes)
		if err != nil {
			return nil, nil, err
		}
		views = append(views, view)
		if selectedName != "" && strings.EqualFold(env.Name, selectedName) {
			copy := view
			selected = &copy
		}
	}
	if selectedName != "" && selected == nil {
		return views, nil, pgx.ErrNoRows
	}
	return views, selected, nil
}

func (h *Handlers) repoActionsEnvironmentView(r *http.Request, environmentID int64, owner, repoName, name, policy string, requiredReviewers, preventSelfReview bool, waitTimer int32) (repoActionsEnvironmentView, error) {
	q := actionsdb.New()
	patternRows, err := q.ListRepoEnvironmentDeploymentBranches(r.Context(), h.d.Pool, environmentID)
	if err != nil {
		return repoActionsEnvironmentView{}, err
	}
	patterns := make([]string, 0, len(patternRows))
	for _, row := range patternRows {
		patterns = append(patterns, row.Pattern)
	}
	secretRows, err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.List(r.Context(), secrets.EnvironmentScope(environmentID))
	if err != nil {
		return repoActionsEnvironmentView{}, err
	}
	return repoActionsEnvironmentView{
		Name:                   name,
		Href:                   repoActionsEnvironmentPath(owner, repoName, name),
		DeploymentBranchPolicy: policy,
		RequiredReviewers:      requiredReviewers,
		PreventSelfReview:      preventSelfReview,
		WaitTimerMinutes:       waitTimer,
		BranchPatterns:         patterns,
		Secrets:                secretRows,
		SecretCount:            len(secretRows),
	}, nil
}

func repoActionsEnvironmentFormFromRequest(r *http.Request, existingName string) (repoActionsEnvironmentForm, error) {
	name := strings.TrimSpace(existingName)
	if name == "" {
		name = strings.TrimSpace(r.PostFormValue("name"))
	}
	if err := validateRepoActionsEnvironmentName(name); err != nil {
		return repoActionsEnvironmentForm{}, err
	}
	policyValue := strings.TrimSpace(r.PostFormValue("deployment_branch_policy"))
	switch policyValue {
	case "all", "protected", "selected":
	default:
		return repoActionsEnvironmentForm{}, errors.New("invalid deployment branch policy")
	}
	waitTimer := int64(0)
	if raw := strings.TrimSpace(r.PostFormValue("wait_timer_minutes")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 0 || n > 43200 {
			return repoActionsEnvironmentForm{}, errors.New("invalid wait timer")
		}
		waitTimer = n
	}
	patterns, patternsText, err := parseRepoActionsEnvironmentBranchPatterns(r.PostFormValue("branch_patterns"))
	if err != nil {
		return repoActionsEnvironmentForm{}, err
	}
	requiredReviewers := formBool(r.PostFormValue("required_reviewers_enabled"))
	return repoActionsEnvironmentForm{
		Name:                   name,
		DeploymentBranchPolicy: policyValue,
		RequiredReviewers:      requiredReviewers,
		PreventSelfReview:      requiredReviewers && formBool(r.PostFormValue("prevent_self_review")),
		WaitTimerMinutes:       int32(waitTimer),
		BranchPatterns:         patterns,
		BranchPatternsText:     patternsText,
	}, nil
}

func formBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

func validateRepoActionsEnvironmentName(name string) error {
	if name == "" {
		return errors.New("environment name is required")
	}
	if len(name) > 255 || strings.Contains(name, "/") || strings.ContainsAny(name, "\x00\r\n\t") {
		return errors.New("environment name must be 255 characters or fewer and cannot contain slashes or control characters")
	}
	return nil
}

func parseRepoActionsEnvironmentBranchPatterns(raw string) ([]string, string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ','
	})
	seen := make(map[string]bool, len(parts))
	patterns := make([]string, 0, len(parts))
	for _, part := range parts {
		pattern := strings.TrimSpace(part)
		if pattern == "" || seen[pattern] {
			continue
		}
		if len(pattern) > 255 {
			return nil, "", errors.New("deployment branch patterns must be 255 characters or fewer")
		}
		seen[pattern] = true
		patterns = append(patterns, pattern)
	}
	return patterns, strings.Join(patterns, "\n"), nil
}

func (h *Handlers) saveRepoActionsEnvironment(r *http.Request, repoID int64, form repoActionsEnvironmentForm) (actionsdb.RepoEnvironment, error) {
	q := actionsdb.New()
	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		return actionsdb.RepoEnvironment{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(r.Context())
		}
	}()
	env, err := q.UpsertRepoEnvironment(r.Context(), tx, actionsdb.UpsertRepoEnvironmentParams{
		RepoID:                   repoID,
		Name:                     form.Name,
		RequiredReviewersEnabled: form.RequiredReviewers,
		PreventSelfReview:        form.PreventSelfReview,
		WaitTimerMinutes:         form.WaitTimerMinutes,
		DeploymentBranchPolicy:   actionsdb.RepoEnvironmentDeploymentBranchPolicy(form.DeploymentBranchPolicy),
	})
	if err != nil {
		return actionsdb.RepoEnvironment{}, err
	}
	if err := q.ReplaceRepoEnvironmentDeploymentBranches(r.Context(), tx, env.ID); err != nil {
		return actionsdb.RepoEnvironment{}, err
	}
	for _, pattern := range form.BranchPatterns {
		if _, err := q.InsertRepoEnvironmentDeploymentBranch(r.Context(), tx, actionsdb.InsertRepoEnvironmentDeploymentBranchParams{
			EnvironmentID: env.ID,
			Pattern:       pattern,
		}); err != nil {
			return actionsdb.RepoEnvironment{}, err
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		return actionsdb.RepoEnvironment{}, err
	}
	committed = true
	return env, nil
}

func repoActionsEnvironmentPath(owner, repoName, name string) string {
	return "/" + owner + "/" + repoName + "/settings/environments/" + url.PathEscape(name)
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
