// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountActionsEnvironments registers the SP23 repository environments REST
// surface. The route shape follows GitHub's repository environments API while
// carrying shithub's current compact protection-rule model.
//
//	GET    /api/v1/repos/{o}/{r}/environments
//	GET    /api/v1/repos/{o}/{r}/environments/{environment}
//	PUT    /api/v1/repos/{o}/{r}/environments/{environment}
//	DELETE /api/v1/repos/{o}/{r}/environments/{environment}
//	GET    /api/v1/repos/{o}/{r}/environments/{environment}/secrets/public-key
//	GET    /api/v1/repos/{o}/{r}/environments/{environment}/secrets
//	GET    /api/v1/repos/{o}/{r}/environments/{environment}/secrets/{name}
//	PUT    /api/v1/repos/{o}/{r}/environments/{environment}/secrets/{name}
//	DELETE /api/v1/repos/{o}/{r}/environments/{environment}/secrets/{name}
func (h *Handlers) mountActionsEnvironments(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/environments", h.actionsEnvironmentsList)
		r.Get("/api/v1/repos/{owner}/{repo}/environments/{environment}", h.actionsEnvironmentsGet)
		r.Get("/api/v1/repos/{owner}/{repo}/environments/{environment}/secrets/public-key", h.actionsEnvironmentSecretsPublicKey)
		r.Get("/api/v1/repos/{owner}/{repo}/environments/{environment}/secrets", h.actionsEnvironmentSecretsList)
		r.Get("/api/v1/repos/{owner}/{repo}/environments/{environment}/secrets/{name}", h.actionsEnvironmentSecretsGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Put("/api/v1/repos/{owner}/{repo}/environments/{environment}", h.actionsEnvironmentsPut)
		r.Delete("/api/v1/repos/{owner}/{repo}/environments/{environment}", h.actionsEnvironmentsDelete)
		r.Put("/api/v1/repos/{owner}/{repo}/environments/{environment}/secrets/{name}", h.actionsEnvironmentSecretsPut)
		r.Delete("/api/v1/repos/{owner}/{repo}/environments/{environment}/secrets/{name}", h.actionsEnvironmentSecretsDelete)
	})
}

type environmentDeploymentBranchPolicyRequest struct {
	ProtectedBranches    bool `json:"protected_branches"`
	CustomBranchPolicies bool `json:"custom_branch_policies"`
}

type environmentDeploymentBranchPolicyResponse struct {
	ProtectedBranches    bool `json:"protected_branches"`
	CustomBranchPolicies bool `json:"custom_branch_policies"`
}

type environmentUpsertRequest struct {
	WaitTimer                *int32                                    `json:"wait_timer,omitempty"`
	RequiredReviewersEnabled *bool                                     `json:"required_reviewers_enabled,omitempty"`
	PreventSelfReview        *bool                                     `json:"prevent_self_review,omitempty"`
	DeploymentBranchPolicy   *environmentDeploymentBranchPolicyRequest `json:"deployment_branch_policy,omitempty"`
	DeploymentPolicyMode     string                                    `json:"deployment_branch_policy_mode,omitempty"`
	BranchPatterns           []string                                  `json:"branch_patterns,omitempty"`
}

type environmentProtectionRuleResponse struct {
	Type              string `json:"type"`
	WaitTimer         int32  `json:"wait_timer,omitempty"`
	PreventSelfReview bool   `json:"prevent_self_review,omitempty"`
}

type environmentResponse struct {
	ID                         int64                                     `json:"id"`
	Name                       string                                    `json:"name"`
	URL                        string                                    `json:"url"`
	HTMLURL                    string                                    `json:"html_url"`
	CreatedAt                  string                                    `json:"created_at"`
	UpdatedAt                  string                                    `json:"updated_at"`
	WaitTimer                  int32                                     `json:"wait_timer"`
	RequiredReviewersEnabled   bool                                      `json:"required_reviewers_enabled"`
	PreventSelfReview          bool                                      `json:"prevent_self_review"`
	DeploymentBranchPolicyMode string                                    `json:"deployment_branch_policy_mode"`
	DeploymentBranchPolicy     environmentDeploymentBranchPolicyResponse `json:"deployment_branch_policy"`
	BranchPatterns             []string                                  `json:"branch_patterns"`
	ProtectionRules            []environmentProtectionRuleResponse       `json:"protection_rules"`
	SecretsURL                 string                                    `json:"secrets_url"`
}

func (h *Handlers) actionsEnvironmentsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	rows, err := actionsdb.New().ListRepoEnvironments(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list environments", "repo_id", repo.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]environmentResponse, 0, len(rows))
	for _, env := range rows {
		item, err := h.presentEnvironment(r, repo, env)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: present environment", "repo_id", repo.ID, "environment_id", env.ID, "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "environments": out})
}

func (h *Handlers) actionsEnvironmentsGet(w http.ResponseWriter, r *http.Request) {
	repo, env, ok := h.resolveAPIEnvironment(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	out, err := h.presentEnvironment(r, repo, env)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: get environment", "repo_id", repo.ID, "environment_id", env.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) actionsEnvironmentsPut(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	if !h.requireAPIRepoFeature(w, r, repo, entitlements.FeatureAdvancedBranchProtection, "Environment protection rules") {
		return
	}
	name := chi.URLParam(r, "environment")
	if err := validateAPIEnvironmentName(name); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var body environmentUpsertRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	q := actionsdb.New()
	existing, err := q.GetRepoEnvironmentByName(r.Context(), h.d.Pool, actionsdb.GetRepoEnvironmentByNameParams{
		RepoID: repo.ID,
		Name:   name,
	})
	exists := true
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			h.d.Logger.ErrorContext(r.Context(), "api: load environment before upsert", "repo_id", repo.ID, "name", name, "error", err)
			writeAPIError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		exists = false
		existing = actionsdb.RepoEnvironment{
			Name:                   name,
			DeploymentBranchPolicy: actionsdb.RepoEnvironmentDeploymentBranchPolicyAll,
		}
	}
	policyMode := existing.DeploymentBranchPolicy
	if body.DeploymentPolicyMode != "" {
		mode, err := parseEnvironmentDeploymentPolicyMode(body.DeploymentPolicyMode)
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		policyMode = mode
	}
	if body.DeploymentBranchPolicy != nil {
		mode, err := parseEnvironmentDeploymentPolicyObject(*body.DeploymentBranchPolicy)
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		policyMode = mode
	}
	waitTimer := existing.WaitTimerMinutes
	if body.WaitTimer != nil {
		if *body.WaitTimer < 0 || *body.WaitTimer > 43200 {
			writeAPIError(w, http.StatusUnprocessableEntity, "wait_timer must be between 0 and 43200")
			return
		}
		waitTimer = *body.WaitTimer
	}
	requiredReviewers := existing.RequiredReviewersEnabled
	if body.RequiredReviewersEnabled != nil {
		requiredReviewers = *body.RequiredReviewersEnabled
	}
	preventSelfReview := existing.PreventSelfReview
	if body.PreventSelfReview != nil {
		preventSelfReview = *body.PreventSelfReview
	}
	if !requiredReviewers {
		preventSelfReview = false
	}
	patterns, err := h.environmentBranchPatternsForUpsert(r, existing.ID, exists, body.BranchPatterns)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	env, err := h.saveAPIEnvironment(r, repo.ID, name, requiredReviewers, preventSelfReview, waitTimer, policyMode, patterns)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: upsert environment", "repo_id", repo.ID, "name", name, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "save failed")
		return
	}
	out, err := h.presentEnvironment(r, repo, env)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: reload environment", "repo_id", repo.ID, "name", name, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	if exists {
		writeJSON(w, http.StatusOK, out)
	} else {
		writeJSON(w, http.StatusCreated, out)
	}
}

func (h *Handlers) actionsEnvironmentsDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	if !h.requireAPIRepoFeature(w, r, repo, entitlements.FeatureAdvancedBranchProtection, "Environment protection rules") {
		return
	}
	name := chi.URLParam(r, "environment")
	if err := actionsdb.New().DeleteRepoEnvironment(r.Context(), h.d.Pool, actionsdb.DeleteRepoEnvironmentParams{
		RepoID: repo.ID,
		Name:   name,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete environment", "repo_id", repo.ID, "name", name, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) actionsEnvironmentSecretsPublicKey(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.resolveAPIEnvironment(w, r, policy.ActionRepoRead); !ok {
		return
	}
	h.writeSecretsPublicKey(w, r)
}

func (h *Handlers) actionsEnvironmentSecretsList(w http.ResponseWriter, r *http.Request) {
	_, env, ok := h.resolveAPIEnvironment(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	rows, err := h.secretsDeps().List(r.Context(), secrets.EnvironmentScope(env.ID))
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list environment secrets", "environment_id", env.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]secretMetaResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, presentSecretMeta(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "secrets": out})
}

func (h *Handlers) actionsEnvironmentSecretsGet(w http.ResponseWriter, r *http.Request) {
	_, env, ok := h.resolveAPIEnvironment(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	rows, err := h.secretsDeps().List(r.Context(), secrets.EnvironmentScope(env.ID))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	for _, m := range rows {
		if m.Name == name {
			writeJSON(w, http.StatusOK, presentSecretMeta(m))
			return
		}
	}
	writeAPIError(w, http.StatusNotFound, "secret not found")
}

func (h *Handlers) actionsEnvironmentSecretsPut(w http.ResponseWriter, r *http.Request) {
	repo, env, ok := h.resolveAPIEnvironment(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	if !h.requireAPIRepoFeature(w, r, repo, entitlements.FeatureActionsOrgSecrets, "Environment Actions secrets") {
		return
	}
	plaintext, ok := h.decodeSecretBody(w, r)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if err := h.secretsDeps().Set(r.Context(), secrets.EnvironmentScope(env.ID), chi.URLParam(r, "name"), plaintext, auth.UserID); err != nil {
		writeSecretsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) actionsEnvironmentSecretsDelete(w http.ResponseWriter, r *http.Request) {
	repo, env, ok := h.resolveAPIEnvironment(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	if !h.requireAPIRepoFeature(w, r, repo, entitlements.FeatureActionsOrgSecrets, "Environment Actions secrets") {
		return
	}
	if err := h.secretsDeps().Delete(r.Context(), secrets.EnvironmentScope(env.ID), chi.URLParam(r, "name")); err != nil {
		writeSecretsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) resolveAPIEnvironment(w http.ResponseWriter, r *http.Request, action policy.Action) (*reposdb.Repo, actionsdb.RepoEnvironment, bool) {
	repo, ok := h.resolveAPIRepo(w, r, action)
	if !ok {
		return nil, actionsdb.RepoEnvironment{}, false
	}
	env, err := actionsdb.New().GetRepoEnvironmentByName(r.Context(), h.d.Pool, actionsdb.GetRepoEnvironmentByNameParams{
		RepoID: repo.ID,
		Name:   chi.URLParam(r, "environment"),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "environment not found")
			return nil, actionsdb.RepoEnvironment{}, false
		}
		h.d.Logger.ErrorContext(r.Context(), "api: lookup environment", "repo_id", repo.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return nil, actionsdb.RepoEnvironment{}, false
	}
	return repo, env, true
}

func (h *Handlers) presentEnvironment(r *http.Request, repo *reposdb.Repo, env actionsdb.RepoEnvironment) (environmentResponse, error) {
	patternRows, err := actionsdb.New().ListRepoEnvironmentDeploymentBranches(r.Context(), h.d.Pool, env.ID)
	if err != nil {
		return environmentResponse{}, err
	}
	patterns := make([]string, 0, len(patternRows))
	for _, row := range patternRows {
		patterns = append(patterns, row.Pattern)
	}
	owner := chi.URLParam(r, "owner")
	repoName := chi.URLParam(r, "repo")
	base := strings.TrimRight(h.d.BaseURL, "/")
	escapedName := url.PathEscape(env.Name)
	path := "/api/v1/repos/" + owner + "/" + repoName + "/environments/" + escapedName
	htmlPath := "/" + owner + "/" + repoName + "/settings/environments/" + escapedName
	rules := make([]environmentProtectionRuleResponse, 0, 2)
	if env.WaitTimerMinutes > 0 {
		rules = append(rules, environmentProtectionRuleResponse{Type: "wait_timer", WaitTimer: env.WaitTimerMinutes})
	}
	if env.RequiredReviewersEnabled {
		rules = append(rules, environmentProtectionRuleResponse{Type: "required_reviewers", PreventSelfReview: env.PreventSelfReview})
	}
	return environmentResponse{
		ID:                         env.ID,
		Name:                       env.Name,
		URL:                        base + path,
		HTMLURL:                    base + htmlPath,
		CreatedAt:                  env.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:                  env.UpdatedAt.Time.UTC().Format(time.RFC3339),
		WaitTimer:                  env.WaitTimerMinutes,
		RequiredReviewersEnabled:   env.RequiredReviewersEnabled,
		PreventSelfReview:          env.PreventSelfReview,
		DeploymentBranchPolicyMode: string(env.DeploymentBranchPolicy),
		DeploymentBranchPolicy:     presentEnvironmentDeploymentPolicy(env.DeploymentBranchPolicy),
		BranchPatterns:             patterns,
		ProtectionRules:            rules,
		SecretsURL:                 base + path + "/secrets",
	}, nil
}

func (h *Handlers) saveAPIEnvironment(r *http.Request, repoID int64, name string, requiredReviewers, preventSelfReview bool, waitTimer int32, policyMode actionsdb.RepoEnvironmentDeploymentBranchPolicy, patterns []string) (actionsdb.RepoEnvironment, error) {
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
		Name:                     name,
		RequiredReviewersEnabled: requiredReviewers,
		PreventSelfReview:        preventSelfReview,
		WaitTimerMinutes:         waitTimer,
		DeploymentBranchPolicy:   policyMode,
	})
	if err != nil {
		return actionsdb.RepoEnvironment{}, err
	}
	if err := q.ReplaceRepoEnvironmentDeploymentBranches(r.Context(), tx, env.ID); err != nil {
		return actionsdb.RepoEnvironment{}, err
	}
	for _, pattern := range patterns {
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

func (h *Handlers) environmentBranchPatternsForUpsert(r *http.Request, environmentID int64, exists bool, raw []string) ([]string, error) {
	if raw != nil || !exists {
		return normalizeEnvironmentBranchPatterns(raw)
	}
	rows, err := actionsdb.New().ListRepoEnvironmentDeploymentBranches(r.Context(), h.d.Pool, environmentID)
	if err != nil {
		return nil, err
	}
	patterns := make([]string, 0, len(rows))
	for _, row := range rows {
		patterns = append(patterns, row.Pattern)
	}
	return patterns, nil
}

func normalizeEnvironmentBranchPatterns(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		pattern := strings.TrimSpace(item)
		if pattern == "" {
			continue
		}
		if len(pattern) > 255 {
			return nil, errors.New("branch_patterns entries must be 255 characters or fewer")
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		out = append(out, pattern)
	}
	return out, nil
}

func validateAPIEnvironmentName(name string) error {
	if name == "" {
		return errors.New("environment name is required")
	}
	if len(name) > 255 || strings.Contains(name, "/") || strings.ContainsAny(name, "\x00\r\n\t") {
		return errors.New("environment name must be 255 characters or fewer and cannot contain slashes or control characters")
	}
	return nil
}

func parseEnvironmentDeploymentPolicyMode(mode string) (actionsdb.RepoEnvironmentDeploymentBranchPolicy, error) {
	switch strings.TrimSpace(mode) {
	case "all":
		return actionsdb.RepoEnvironmentDeploymentBranchPolicyAll, nil
	case "protected":
		return actionsdb.RepoEnvironmentDeploymentBranchPolicyProtected, nil
	case "selected":
		return actionsdb.RepoEnvironmentDeploymentBranchPolicySelected, nil
	default:
		return "", errors.New("deployment_branch_policy_mode must be all, protected, or selected")
	}
}

func parseEnvironmentDeploymentPolicyObject(policy environmentDeploymentBranchPolicyRequest) (actionsdb.RepoEnvironmentDeploymentBranchPolicy, error) {
	switch {
	case policy.ProtectedBranches && policy.CustomBranchPolicies:
		return "", errors.New("deployment_branch_policy cannot set both protected_branches and custom_branch_policies")
	case policy.ProtectedBranches:
		return actionsdb.RepoEnvironmentDeploymentBranchPolicyProtected, nil
	case policy.CustomBranchPolicies:
		return actionsdb.RepoEnvironmentDeploymentBranchPolicySelected, nil
	default:
		return actionsdb.RepoEnvironmentDeploymentBranchPolicyAll, nil
	}
}

func presentEnvironmentDeploymentPolicy(mode actionsdb.RepoEnvironmentDeploymentBranchPolicy) environmentDeploymentBranchPolicyResponse {
	switch mode {
	case actionsdb.RepoEnvironmentDeploymentBranchPolicyProtected:
		return environmentDeploymentBranchPolicyResponse{ProtectedBranches: true}
	case actionsdb.RepoEnvironmentDeploymentBranchPolicySelected:
		return environmentDeploymentBranchPolicyResponse{CustomBranchPolicies: true}
	default:
		return environmentDeploymentBranchPolicyResponse{}
	}
}

func (h *Handlers) requireAPIRepoFeature(w http.ResponseWriter, r *http.Request, repo *reposdb.Repo, feature entitlements.Feature, label string) bool {
	if !repo.OwnerOrgID.Valid || repo.Visibility != reposdb.RepoVisibilityPrivate {
		return true
	}
	decision, err := entitlements.CheckOrgFeature(r.Context(), entitlements.Deps{Pool: h.d.Pool}, repo.OwnerOrgID.Int64, feature)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: repo feature gate", "repo_id", repo.ID, "feature", feature, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "entitlement check failed")
		return false
	}
	if decision.Allowed {
		return true
	}
	banner := decision.UpgradeBanner(label, chi.URLParam(r, "owner"))
	writeAPIError(w, banner.StatusCode, banner.Message)
	return false
}
