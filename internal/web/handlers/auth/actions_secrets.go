// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	"github.com/tenseleyFlow/shithub/internal/actions/variables"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-12: personal Actions secrets + variables settings page.
//
// Free users see the page with the form Pro-locked. The user-scope rows
// in workflow_secrets / actions_variables are gated by
// FeatureUserActionsSecrets — write-path gating prevents Free users
// from creating user-scope rows, and PRO-EXT01-12b's runner-side filter
// (still to come) will exclude them from job dispatch when enforce is
// flipped.

// settingsActionsSecretsForm renders GET /settings/actions/secrets.
func (h *Handlers) settingsActionsSecretsForm(w http.ResponseWriter, r *http.Request) {
	h.renderActionsSecretsPage(w, r, "", "", "")
}

func (h *Handlers) renderActionsSecretsPage(
	w http.ResponseWriter, r *http.Request,
	createError, secretFormName, varFormName string,
) {
	user := middleware.CurrentUserFromContext(r.Context())
	allowed, _, _ := h.userActionsSecretsAllowed(r.Context(), user.ID)

	var secretMetas []secrets.Meta
	if h.d.SecretBox != nil {
		var err error
		secretMetas, err = secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox, Logger: h.d.Logger}.
			List(r.Context(), secrets.UserScope(user.ID))
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "actions secrets: list", "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}
	varRows, err := variables.Deps{Pool: h.d.Pool}.List(r.Context(), variables.UserScope(user.ID))
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "actions variables: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	h.renderPage(w, r, "settings/actions_secrets", map[string]any{
		"Title":          "Personal Actions secrets",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "actions-secrets",
		"Secrets":        secretMetas,
		"Variables":      varRows,
		"Allowed":        allowed,
		"FineGrainedKey": string(entitlements.FeatureUserActionsSecrets),
		"CreateError":    createError,
		"SecretFormName": secretFormName,
		"VarFormName":    varFormName,
	})
}

// userActionsSecretsAllowed wraps the entitlement check + report-only
// logging. Same shape as the other Pro gates.
func (h *Handlers) userActionsSecretsAllowed(ctx context.Context, userID int64) (bool, entitlements.Decision, error) {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID),
		entitlements.FeatureUserActionsSecrets)
	if err != nil {
		return false, entitlements.Decision{}, err
	}
	if !decision.Allowed {
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", billing.PrincipalForUser(userID).String(),
			"principal_kind", string(billing.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureUserActionsSecrets),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", "report_only")
	}
	return decision.Allowed, decision, nil
}

// settingsActionsSecretCreate handles POST /settings/actions/secrets.
// Gates writes on FeatureUserActionsSecrets when enforce is on; in
// report-only the write succeeds (and the would-deny is logged).
func (h *Handlers) settingsActionsSecretCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	name := strings.TrimSpace(r.PostFormValue("name"))
	value := r.PostFormValue("value")

	allowed, decision, derr := h.userActionsSecretsAllowed(r.Context(), user.ID)
	if derr != nil {
		h.d.Logger.ErrorContext(r.Context(), "actions secrets: entitlement", "error", derr)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !allowed && h.d.BillingEnforce.UserActionsSecrets {
		banner := decision.PrincipalUpgradeBanner("personal Actions secrets",
			billing.PrincipalForUser(user.ID), "")
		h.renderActionsSecretsPage(w, r, banner.Message, name, "")
		return
	}

	if h.d.SecretBox == nil {
		h.renderActionsSecretsPage(w, r,
			"Operator has not configured a secret box; cannot store secrets.", name, "")
		return
	}

	err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox, Logger: h.d.Logger}.
		Set(r.Context(), secrets.UserScope(user.ID), name, []byte(value), user.ID)
	switch {
	case errors.Is(err, secrets.ErrInvalidName):
		h.renderActionsSecretsPage(w, r,
			"Name must be 1-100 characters, start with a letter or underscore, and contain only letters, digits, and underscores.", name, "")
		return
	case errors.Is(err, secrets.ErrEmptyValue):
		h.renderActionsSecretsPage(w, r, "Secret value cannot be empty.", name, "")
		return
	case err != nil:
		h.d.Logger.ErrorContext(r.Context(), "actions secrets: set", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	http.Redirect(w, r, "/settings/actions/secrets", http.StatusSeeOther)
}

// settingsActionsSecretDelete handles POST /settings/actions/secrets/{name}/delete.
func (h *Handlers) settingsActionsSecretDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	name := chi.URLParam(r, "name")

	if h.d.SecretBox != nil {
		deps := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox, Logger: h.d.Logger}
		if err := deps.Delete(r.Context(), secrets.UserScope(user.ID), name); err != nil &&
			!errors.Is(err, secrets.ErrNotFound) {
			h.d.Logger.ErrorContext(r.Context(), "actions secrets: delete", "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}
	http.Redirect(w, r, "/settings/actions/secrets", http.StatusSeeOther)
}

// settingsActionsVariableCreate handles POST /settings/actions/variables.
// Same gate as secrets — these features share the Pro umbrella.
func (h *Handlers) settingsActionsVariableCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	name := strings.TrimSpace(r.PostFormValue("name"))
	value := r.PostFormValue("value")

	allowed, decision, derr := h.userActionsSecretsAllowed(r.Context(), user.ID)
	if derr != nil {
		h.d.Logger.ErrorContext(r.Context(), "actions variables: entitlement", "error", derr)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !allowed && h.d.BillingEnforce.UserActionsSecrets {
		banner := decision.PrincipalUpgradeBanner("personal Actions variables",
			billing.PrincipalForUser(user.ID), "")
		h.renderActionsSecretsPage(w, r, banner.Message, "", name)
		return
	}

	err := variables.Deps{Pool: h.d.Pool}.
		Set(r.Context(), variables.UserScope(user.ID), name, value, user.ID)
	switch {
	case errors.Is(err, variables.ErrInvalidName):
		h.renderActionsSecretsPage(w, r,
			"Variable name must be 1-100 characters, start with a letter or underscore, and contain only letters, digits, and underscores.", "", name)
		return
	case errors.Is(err, variables.ErrValueTooLong):
		h.renderActionsSecretsPage(w, r, "Variable value must be 4096 characters or fewer.", "", name)
		return
	case err != nil:
		h.d.Logger.ErrorContext(r.Context(), "actions variables: set", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	http.Redirect(w, r, "/settings/actions/secrets", http.StatusSeeOther)
}

// settingsActionsVariableDelete handles POST /settings/actions/variables/{name}/delete.
func (h *Handlers) settingsActionsVariableDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	name := chi.URLParam(r, "name")

	deps := variables.Deps{Pool: h.d.Pool}
	if err := deps.Delete(r.Context(), variables.UserScope(user.ID), name); err != nil &&
		!errors.Is(err, variables.ErrNotFound) {
		h.d.Logger.ErrorContext(r.Context(), "actions variables: delete", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/actions/secrets", http.StatusSeeOther)
}
