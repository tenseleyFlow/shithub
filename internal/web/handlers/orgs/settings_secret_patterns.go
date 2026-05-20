// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/secretscan"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type secretPatternForm struct {
	Name        string
	Description string
	Pattern     string
	MinMatchLen int
}

func (h *Handlers) settingsSecretPatterns(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	h.renderOrgSecretPatternsSettings(w, r, org, secretPatternForm{MinMatchLen: secretscan.CustomPatternMinMatch}, "", orgSecretPatternsNotice(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsSecretPatternCreate(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if !h.orgSecretPatternsFeatureAllowed(w, r, org) {
		return
	}
	form, err := parseSecretPatternForm(r)
	if err != nil {
		h.renderOrgSecretPatternsSettings(w, r, org, form, err.Error(), "")
		return
	}
	if _, err := secretscan.CompileCustomPattern(form.toSpec()); err != nil {
		h.renderOrgSecretPatternsSettings(w, r, org, form, friendlyCustomPatternError(err), "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := secretscandb.New().CreateSecretScanCustomPattern(r.Context(), h.d.Pool, secretscandb.CreateSecretScanCustomPatternParams{
		OrgID:       org.ID,
		Name:        strings.TrimSpace(form.Name),
		Description: strings.TrimSpace(form.Description),
		Pattern:     strings.TrimSpace(form.Pattern),
		MinMatchLen: int32(form.MinMatchLen),
		CreatedBy:   pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.renderOrgSecretPatternsSettings(w, r, org, form, friendlyCustomPatternSaveError(err), "")
		return
	}
	http.Redirect(w, r, orgSecretPatternsPath(org.Slug)+"?notice=created", http.StatusSeeOther)
}

func (h *Handlers) settingsSecretPatternUpdate(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if !h.orgSecretPatternsFeatureAllowed(w, r, org) {
		return
	}
	patternID, ok := parseSecretPatternID(w, r)
	if !ok {
		return
	}
	form, err := parseSecretPatternForm(r)
	if err != nil {
		h.renderOrgSecretPatternsSettings(w, r, org, form, err.Error(), "")
		return
	}
	if _, err := secretscan.CompileCustomPattern(form.toSpec()); err != nil {
		h.renderOrgSecretPatternsSettings(w, r, org, form, friendlyCustomPatternError(err), "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := secretscandb.New().UpdateSecretScanCustomPattern(r.Context(), h.d.Pool, secretscandb.UpdateSecretScanCustomPatternParams{
		ID:          patternID,
		OrgID:       org.ID,
		Name:        strings.TrimSpace(form.Name),
		Description: strings.TrimSpace(form.Description),
		Pattern:     strings.TrimSpace(form.Pattern),
		MinMatchLen: int32(form.MinMatchLen),
		UpdatedBy:   pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.renderOrgSecretPatternsSettings(w, r, org, form, friendlyCustomPatternSaveError(err), "")
		return
	}
	http.Redirect(w, r, orgSecretPatternsPath(org.Slug)+"?notice=updated", http.StatusSeeOther)
}

func (h *Handlers) settingsSecretPatternEnable(w http.ResponseWriter, r *http.Request) {
	h.settingsSecretPatternSetEnabled(w, r, true)
}

func (h *Handlers) settingsSecretPatternDisable(w http.ResponseWriter, r *http.Request) {
	h.settingsSecretPatternSetEnabled(w, r, false)
}

func (h *Handlers) settingsSecretPatternSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if !h.orgSecretPatternsFeatureAllowed(w, r, org) {
		return
	}
	patternID, ok := parseSecretPatternID(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := secretscandb.New().SetSecretScanCustomPatternEnabled(r.Context(), h.d.Pool, secretscandb.SetSecretScanCustomPatternEnabledParams{
		ID:        patternID,
		OrgID:     org.ID,
		Enabled:   enabled,
		UpdatedBy: pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.renderOrgSecretPatternsSettings(w, r, org, secretPatternForm{MinMatchLen: secretscan.CustomPatternMinMatch}, "Could not update that pattern.", "")
		return
	}
	notice := "disabled"
	if enabled {
		notice = "enabled"
	}
	http.Redirect(w, r, orgSecretPatternsPath(org.Slug)+"?notice="+notice, http.StatusSeeOther)
}

func (h *Handlers) settingsSecretPatternDelete(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if !h.orgSecretPatternsFeatureAllowed(w, r, org) {
		return
	}
	patternID, ok := parseSecretPatternID(w, r)
	if !ok {
		return
	}
	if err := secretscandb.New().DeleteSecretScanCustomPattern(r.Context(), h.d.Pool, secretscandb.DeleteSecretScanCustomPatternParams{
		ID:    patternID,
		OrgID: org.ID,
	}); err != nil {
		h.renderOrgSecretPatternsSettings(w, r, org, secretPatternForm{MinMatchLen: secretscan.CustomPatternMinMatch}, "Could not delete that pattern.", "")
		return
	}
	http.Redirect(w, r, orgSecretPatternsPath(org.Slug)+"?notice=deleted", http.StatusSeeOther)
}

func (h *Handlers) orgSecretPatternsFeatureAllowed(w http.ResponseWriter, r *http.Request, org orgsdb.Org) bool {
	decision, err := entitlements.CheckOrgFeature(r.Context(), entitlements.Deps{Pool: h.d.Pool}, org.ID, entitlements.FeatureSecretCustomPatterns)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org secret patterns: entitlement check", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return false
	}
	if decision.Allowed {
		return true
	}
	w.WriteHeader(decision.HTTPStatus())
	h.renderOrgSecretPatternsSettings(w, r, org, secretPatternForm{MinMatchLen: secretscan.CustomPatternMinMatch}, decision.UpgradeBanner("Custom secret patterns", string(org.Slug)).Message, "")
	return false
}

func (h *Handlers) renderOrgSecretPatternsSettings(w http.ResponseWriter, r *http.Request, org orgsdb.Org, form secretPatternForm, errMsg, notice string) {
	decision, err := entitlements.CheckOrgFeature(r.Context(), entitlements.Deps{Pool: h.d.Pool}, org.ID, entitlements.FeatureSecretCustomPatterns)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org secret patterns: entitlement check", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	var patterns []secretscandb.SecretScanCustomPattern
	if decision.Allowed {
		patterns, err = secretscandb.New().ListSecretScanCustomPatternsForOrg(r.Context(), h.d.Pool, org.ID)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "org secret patterns: list", "org_id", org.ID, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}
	if form.MinMatchLen == 0 {
		form.MinMatchLen = secretscan.CustomPatternMinMatch
	}
	disabledMessage := ""
	if !decision.Allowed {
		disabledMessage = decision.UpgradeBanner("Custom secret patterns", string(org.Slug)).Message
	}
	data := map[string]any{
		"Title":                 org.Slug + " · Custom secret patterns",
		"Org":                   org,
		"CSRFToken":             middleware.CSRFTokenForRequest(r),
		"OrgSettingsActive":     "secret-patterns",
		"BillingEnabled":        h.billingConfigured(),
		"Error":                 errMsg,
		"Notice":                notice,
		"Form":                  form,
		"Patterns":              patterns,
		"WritesDisabled":        !decision.Allowed,
		"WritesDisabledMessage": disabledMessage,
		"FeatureKey":            string(entitlements.FeatureSecretCustomPatterns),
		"MinPatternMatchLen":    secretscan.CustomPatternMinMatch,
		"MaxPatternMatchLen":    secretscan.CustomPatternMaxMatch,
	}
	h.d.Render.RenderPage(w, r, "orgs/settings_secret_patterns", data)
}

func parseSecretPatternForm(r *http.Request) (secretPatternForm, error) {
	if err := r.ParseForm(); err != nil {
		return secretPatternForm{MinMatchLen: secretscan.CustomPatternMinMatch}, errors.New("could not parse form")
	}
	form := secretPatternForm{
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Pattern:     strings.TrimSpace(r.PostFormValue("pattern")),
		MinMatchLen: secretscan.CustomPatternMinMatch,
	}
	if raw := strings.TrimSpace(r.PostFormValue("min_match_len")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return form, errors.New("minimum match length must be a number")
		}
		form.MinMatchLen = n
	}
	return form, nil
}

func (f secretPatternForm) toSpec() secretscan.CustomPatternSpec {
	return secretscan.CustomPatternSpec{
		Name:        f.Name,
		Description: f.Description,
		Pattern:     f.Pattern,
		MinMatchLen: f.MinMatchLen,
	}
}

func parseSecretPatternID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "patternID"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad pattern id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func orgSecretPatternsPath(slug string) string {
	return "/organizations/" + slug + "/settings/security/secret-patterns"
}

func friendlyCustomPatternError(err error) string {
	switch {
	case errors.Is(err, secretscan.ErrCustomPatternNameRequired),
		errors.Is(err, secretscan.ErrCustomPatternNameInvalid),
		errors.Is(err, secretscan.ErrCustomPatternNameTooLong),
		errors.Is(err, secretscan.ErrCustomPatternNameReserved),
		errors.Is(err, secretscan.ErrCustomPatternDescriptionTooLong),
		errors.Is(err, secretscan.ErrCustomPatternExpressionRequired),
		errors.Is(err, secretscan.ErrCustomPatternExpressionTooLong),
		errors.Is(err, secretscan.ErrCustomPatternMatchesEmpty),
		errors.Is(err, secretscan.ErrCustomPatternMinMatchInvalid):
		return err.Error()
	case errors.Is(err, secretscan.ErrCustomPatternExpressionInvalid):
		return "Pattern must be a valid Go regular expression."
	default:
		return "Could not save custom pattern."
	}
}

func friendlyCustomPatternSaveError(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return "A custom pattern with that name already exists."
	}
	return "Could not save custom pattern."
}

func orgSecretPatternsNotice(code string) string {
	switch code {
	case "created":
		return "Custom pattern created."
	case "updated":
		return "Custom pattern updated."
	case "enabled":
		return "Custom pattern enabled."
	case "disabled":
		return "Custom pattern disabled."
	case "deleted":
		return "Custom pattern deleted."
	default:
		return ""
	}
}
