// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

// PRO-EXT01-10d: settings page at /settings/secret-scanning/alerts.
// The form mirrors the digest pattern: GET shows the current row (or
// the empty default for a never-opted-in user); POST upserts; a
// separate "disable" POST deletes the row (the at_least_one CHECK
// rejects an empty row, so the explicit delete path is the clean off
// state).

import (
	"crypto/rand"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// secretScanAlertsForm is the GET render state.
type secretScanAlertsForm struct {
	EmailEnabled    bool
	WebhookURL      string
	HasWebhookSet   bool   // true when a secret has been generated
	WebhookRotation string // displayed once after a save that minted a new secret
}

func (h *Handlers) settingsSecretScanAlertsForm(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	form := secretScanAlertsForm{}
	row, err := secretscandb.New().GetSecretScanAlertPrefs(r.Context(), h.d.Pool, user.ID)
	switch {
	case err == nil:
		form.EmailEnabled = row.EmailEnabled
		if row.WebhookUrl.Valid {
			form.WebhookURL = row.WebhookUrl.String
		}
		form.HasWebhookSet = len(row.WebhookSecret) > 0
	case errors.Is(err, pgx.ErrNoRows):
		// Empty form is the default
	default:
		h.d.Logger.ErrorContext(r.Context(), "ssa: load", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderSecretScanAlertsForm(w, r, form, "", "")
}

func (h *Handlers) settingsSecretScanAlertsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	if !h.secretScanAlertsGate(w, r, user.ID, "save") {
		return
	}

	emailEnabled := r.PostFormValue("email_enabled") == "on"
	webhookURL := strings.TrimSpace(r.PostFormValue("webhook_url"))

	// Validate webhook URL shape client-side too — the CHECK
	// constraint will reject malformed values but the friendly error
	// here saves a 500.
	if webhookURL != "" {
		u, perr := url.Parse(webhookURL)
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			h.renderSecretScanAlertsForm(w, r,
				secretScanAlertsForm{EmailEnabled: emailEnabled, WebhookURL: webhookURL},
				"Webhook URL must be a http(s) URL with a host.", "")
			return
		}
	}
	if !emailEnabled && webhookURL == "" {
		h.renderSecretScanAlertsForm(w, r,
			secretScanAlertsForm{},
			"Enable at least one channel (email or webhook).", "")
		return
	}

	// Webhook secret rotation: if the URL changes (or is being set for
	// the first time), mint a new HMAC key. Surface it ONCE in the
	// next render so the user can copy it into their endpoint config.
	existing, _ := secretscandb.New().GetSecretScanAlertPrefs(r.Context(), h.d.Pool, user.ID)
	var (
		webhookSecret []byte
		rotated       string
	)
	if webhookURL != "" {
		needNew := !existing.WebhookUrl.Valid ||
			existing.WebhookUrl.String != webhookURL ||
			len(existing.WebhookSecret) == 0
		if needNew {
			webhookSecret = make([]byte, 32)
			if _, rerr := rand.Read(webhookSecret); rerr != nil {
				h.d.Logger.ErrorContext(r.Context(), "ssa: gen secret", "error", rerr)
				h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
				return
			}
			rotated = encodeHex(webhookSecret)
		} else {
			webhookSecret = existing.WebhookSecret
		}
	}

	params := secretscandb.UpsertSecretScanAlertPrefsParams{
		UserID:        user.ID,
		EmailEnabled:  emailEnabled,
		WebhookUrl:    pgtype.Text{},
		WebhookSecret: nil,
	}
	if webhookURL != "" {
		params.WebhookUrl = pgtype.Text{String: webhookURL, Valid: true}
		params.WebhookSecret = webhookSecret
	}
	if _, err := secretscandb.New().UpsertSecretScanAlertPrefs(r.Context(), h.d.Pool, params); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "ssa: upsert", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderSecretScanAlertsForm(w, r, secretScanAlertsForm{
		EmailEnabled:    emailEnabled,
		WebhookURL:      webhookURL,
		HasWebhookSet:   webhookURL != "",
		WebhookRotation: rotated,
	}, "", "Saved.")
}

func (h *Handlers) settingsSecretScanAlertsDisable(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	if err := secretscandb.New().DeleteSecretScanAlertPrefs(r.Context(), h.d.Pool, user.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "ssa: delete", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/secret-scanning/alerts", http.StatusSeeOther)
}

func (h *Handlers) secretScanAlertsGate(w http.ResponseWriter, r *http.Request, userID int64, action string) bool {
	principal := billing.PrincipalForUser(userID)
	decision, err := entitlements.CheckPrincipalFeature(r.Context(),
		entitlements.Deps{Pool: h.d.Pool}, principal, entitlements.FeatureSecretScanAlerts)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "ssa gate: check", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return false
	}
	if !decision.Allowed {
		mode := "report_only"
		if h.d.BillingEnforce.UserSecretScanAlerts {
			mode = "enforce"
		}
		h.d.Logger.InfoContext(r.Context(), "entitlements.report_only_deny",
			"principal", principal.String(),
			"principal_kind", string(principal.Kind),
			"principal_id", userID,
			"feature", string(entitlements.FeatureSecretScanAlerts),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "settings-secret-scan-alerts",
			"action", action)
		if h.d.BillingEnforce.UserSecretScanAlerts {
			http.Error(w,
				"secret-scan alerts require a Pro subscription — see /settings/billing",
				http.StatusPaymentRequired)
			return false
		}
	}
	return true
}

func (h *Handlers) renderSecretScanAlertsForm(w http.ResponseWriter, r *http.Request, form secretScanAlertsForm, errMsg, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	allowed := h.secretScanAlertsAllowedRead(r, user.ID)
	h.renderPage(w, r, "settings/secret_scan_alerts", map[string]any{
		"Title":          "Secret scan alerts",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "secret-scan-alerts",
		"Form":           form,
		"AlertsAllowed":  allowed,
		"FeatureKey":     string(entitlements.FeatureSecretScanAlerts),
		"Error":          errMsg,
		"Success":        successMsg,
	})
}

// secretScanAlertsAllowedRead is the silent renderer check (no
// would-deny log emission) used to toggle the Pro-lock wrapper on
// every page view. The save/disable handlers emit the deny telemetry
// from the gate; rendering must not flood it.
func (h *Handlers) secretScanAlertsAllowedRead(r *http.Request, userID int64) bool {
	decision, err := entitlements.CheckPrincipalFeature(r.Context(),
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID), entitlements.FeatureSecretScanAlerts)
	if err != nil {
		return false
	}
	return decision.Allowed
}

// encodeHex avoids importing encoding/hex in this file's header just
// for one call site — keeps the import block lean since we already
// pulled crypto/rand for the same path.
func encodeHex(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out)
}
