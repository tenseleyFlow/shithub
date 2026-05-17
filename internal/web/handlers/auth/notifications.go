// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"encoding/json"
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// notifChannel is one toggleable preference. Adding a channel is a code
// change (this list) plus a translation, never a DB migration; the
// underlying user_notification_prefs table is generic key/value.
type notifChannel struct {
	Key         string
	Title       string
	Description string
	Required    bool // when true, displayed but not toggleable
}

// notifChannels is the fixed set surfaced on /settings/notifications.
// Order matters — it's the render order. Required channels (security
// alerts) are listed first so they're prominent.
var notifChannels = []notifChannel{
	{
		Key:         "security_alerts",
		Title:       "Security alerts",
		Description: "New sign-ins, password changes, 2FA enrollment, recovery codes used. These cannot be disabled.",
		Required:    true,
	},
	{
		Key:         "account_changes",
		Title:       "Account changes",
		Description: "Username changes, primary-email switches, account-deletion confirmations.",
	},
	{
		Key:         "product_news",
		Title:       "Product news",
		Description: "Occasional updates about new shithub features. Off by default.",
	},
}

// notifChannelDefault returns the default state for a channel that has
// no DB row. Account-related channels are opt-out (default on); marketing
// channels are opt-in (default off).
func notifChannelDefault(key string) bool {
	switch key {
	case "security_alerts", "account_changes":
		return true
	default:
		return false
	}
}

// notifPrefValue is the JSON shape we persist in user_notification_prefs.value.
type notifPrefValue struct {
	Enabled bool `json:"enabled"`
}

// settingsNotificationsForm renders GET /settings/notifications.
func (h *Handlers) settingsNotificationsForm(w http.ResponseWriter, r *http.Request) {
	h.renderNotificationsForm(w, r, "")
}

// settingsNotificationsSubmit handles POST /settings/notifications.
//
// Diff strategy:
//   - For each non-required channel, read the form checkbox.
//   - If desired matches default → DeleteUserNotificationPref (so the
//     row only exists when the user has actively diverged).
//   - Otherwise → UpsertUserNotificationPref with {"enabled":<desired>}.
//
// This keeps the table small and makes "reset to defaults" a no-op
// rather than a code change.
func (h *Handlers) settingsNotificationsSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())

	for _, ch := range notifChannels {
		if ch.Required {
			continue
		}
		desired := r.PostFormValue(ch.Key) == "on"
		if desired == notifChannelDefault(ch.Key) {
			if err := h.q.DeleteUserNotificationPref(r.Context(), h.d.Pool, usersdb.DeleteUserNotificationPrefParams{
				UserID: user.ID, Key: ch.Key,
			}); err != nil {
				h.d.Logger.ErrorContext(r.Context(), "notif: delete", "error", err, "key", ch.Key)
				h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
				return
			}
			continue
		}
		val, err := json.Marshal(notifPrefValue{Enabled: desired})
		if err != nil {
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		if err := h.q.UpsertUserNotificationPref(r.Context(), h.d.Pool, usersdb.UpsertUserNotificationPrefParams{
			UserID: user.ID, Key: ch.Key, Value: val,
		}); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "notif: upsert", "error", err, "key", ch.Key)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}

	h.renderNotificationsForm(w, r, "Preferences saved.")
}

// renderNotificationsForm pulls the current persisted state, layers it
// on top of defaults, and renders the toggles.
func (h *Handlers) renderNotificationsForm(w http.ResponseWriter, r *http.Request, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	rows, err := h.q.ListUserNotificationPrefs(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notif: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	persisted := make(map[string]bool, len(rows))
	for _, p := range rows {
		var v notifPrefValue
		if err := json.Unmarshal(p.Value, &v); err != nil {
			continue
		}
		persisted[p.Key] = v.Enabled
	}

	type viewChan struct {
		notifChannel
		Enabled bool
	}
	view := make([]viewChan, 0, len(notifChannels))
	for _, ch := range notifChannels {
		enabled := notifChannelDefault(ch.Key)
		if v, ok := persisted[ch.Key]; ok {
			enabled = v
		}
		if ch.Required {
			enabled = true
		}
		view = append(view, viewChan{notifChannel: ch, Enabled: enabled})
	}

	rules, _ := notifdb.New().ListUserNotificationRules(r.Context(), h.d.Pool, user.ID)
	rulesAllowed := h.inboxRulesAllowed(r, user.ID)

	h.renderPage(w, r, "settings/notifications", map[string]any{
		"Title":              "Notifications",
		"CSRFToken":          middleware.CSRFTokenForRequest(r),
		"SettingsActive":     "notifications",
		"Channels":           view,
		"Success":            successMsg,
		"Rules":              rules,
		"RulesAllowed":       rulesAllowed,
		"RulesFeatureKey":    string(entitlements.FeatureInboxRules),
		"RuleActionSnooze":   string(notifdb.UserNotificationRuleActionSnooze),
		"RuleActionTab":      string(notifdb.UserNotificationRuleActionTab),
		"RuleActionMarkRead": string(notifdb.UserNotificationRuleActionMarkRead),
		"RuleActionDrop":     string(notifdb.UserNotificationRuleActionDrop),
	})
}

// inboxRulesAllowed reports whether the user's entitlement permits
// rule creation. Used to decide whether the "Add rule" form renders
// enabled or disabled-with-Pro-tooltip. Failures fail closed so a
// blip doesn't accidentally unlock the gated affordance.
func (h *Handlers) inboxRulesAllowed(r *http.Request, userID int64) bool {
	decision, err := entitlements.CheckPrincipalFeature(r.Context(),
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID), entitlements.FeatureInboxRules)
	if err != nil {
		return false
	}
	return decision.Allowed
}
