// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-16a: notification routing rules CRUD on the existing
// /settings/notifications page. Three routes:
//
//	POST /settings/notifications/rules           — create
//	POST /settings/notifications/rules/{id}/delete  — remove
//	POST /settings/notifications/rules/{id}/toggle  — enable/disable
//
// The list + create form are rendered inline by the existing
// settingsNotificationsForm handler (notification_rules_render.go
// folds the extra view data in). Free users see the form disabled
// with a Pro lock badge.

// settingsRuleCreate handles POST /settings/notifications/rules.
// Validates the action payload + entitlement-gates Free users when
// enforce is on.
func (h *Handlers) settingsRuleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	if !h.notifRuleGate(w, r, user.ID, "create") {
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" || len(name) > 120 {
		h.renderNotificationsForm(w, r, "")
		return
	}

	action := notifdb.UserNotificationRuleAction(r.PostFormValue("action"))
	switch action {
	case notifdb.UserNotificationRuleActionSnooze,
		notifdb.UserNotificationRuleActionTab,
		notifdb.UserNotificationRuleActionMarkRead,
		notifdb.UserNotificationRuleActionDrop:
	default:
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}

	params := notifdb.InsertUserNotificationRuleParams{
		UserID:  user.ID,
		Name:    name,
		Enabled: true,
		Action:  action,
	}

	// Match dimensions — all optional; empty/zero → NULL = no filter.
	if v := strings.TrimSpace(r.PostFormValue("match_reason")); v != "" {
		params.MatchReason = pgtype.Text{String: v, Valid: true}
	}
	if v := strings.TrimSpace(r.PostFormValue("match_kind")); v != "" {
		params.MatchKind = pgtype.Text{String: v, Valid: true}
	}
	if v := strings.TrimSpace(r.PostFormValue("match_repo_id")); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			params.MatchRepoID = pgtype.Int8{Int64: id, Valid: true}
		}
	}
	if v := strings.TrimSpace(r.PostFormValue("match_actor_id")); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			params.MatchActorID = pgtype.Int8{Int64: id, Valid: true}
		}
	}

	// Action params — validated against the DB constraint shape.
	switch action {
	case notifdb.UserNotificationRuleActionSnooze:
		v := strings.TrimSpace(r.PostFormValue("action_snooze_minutes"))
		mins, err := strconv.Atoi(v)
		if err != nil || mins <= 0 {
			http.Error(w, "snooze action requires positive minutes", http.StatusBadRequest)
			return
		}
		params.ActionSnoozeMinutes = pgtype.Int4{Int32: int32(mins), Valid: true}
	case notifdb.UserNotificationRuleActionTab:
		v := strings.TrimSpace(r.PostFormValue("action_tab"))
		if v == "" || len(v) > 64 {
			http.Error(w, "tab action requires a label (1-64 chars)", http.StatusBadRequest)
			return
		}
		params.ActionTab = pgtype.Text{String: v, Valid: true}
	}

	// Position is always max+1 — the user can reorder later (deferred
	// to a follow-up sprint; for now just append).
	pos, err := notifdb.New().NextUserNotificationRulePosition(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notif rules: next position", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	params.Position = pos

	if _, err := notifdb.New().InsertUserNotificationRule(r.Context(), h.d.Pool, params); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notif rules: insert", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/notifications", http.StatusSeeOther)
}

// settingsRuleDelete handles POST /settings/notifications/rules/{id}/delete.
func (h *Handlers) settingsRuleDelete(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	id, ok := parseRuleID(w, r)
	if !ok {
		return
	}
	n, err := notifdb.New().DeleteUserNotificationRule(r.Context(), h.d.Pool, notifdb.DeleteUserNotificationRuleParams{
		ID: id, UserID: user.ID,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notif rules: delete", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if n == 0 {
		// Collapse "not found" + "not yours" into a single 404 so
		// attackers can't probe rule IDs across users.
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	http.Redirect(w, r, "/settings/notifications", http.StatusSeeOther)
}

// settingsRuleToggle flips the enabled flag. POST-only.
func (h *Handlers) settingsRuleToggle(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	id, ok := parseRuleID(w, r)
	if !ok {
		return
	}
	// Read current state to compute the flip — atomic-ness here is
	// less important than the audit trail showing the user toggled
	// it (a concurrent toggle from another tab is benign).
	rule, err := notifdb.New().GetUserNotificationRule(r.Context(), h.d.Pool, notifdb.GetUserNotificationRuleParams{
		ID: id, UserID: user.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "notif rules: get", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	n, err := notifdb.New().SetUserNotificationRuleEnabled(r.Context(), h.d.Pool, notifdb.SetUserNotificationRuleEnabledParams{
		ID: id, UserID: user.ID, Enabled: !rule.Enabled,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notif rules: toggle", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if n == 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	http.Redirect(w, r, "/settings/notifications", http.StatusSeeOther)
}

// notifRuleGate enforces FeatureInboxRules at the create site.
// Toggle + delete are NOT gated — a Pro→Free downgrade must not
// leave the user unable to remove their own rules.
func (h *Handlers) notifRuleGate(w http.ResponseWriter, r *http.Request, userID int64, action string) bool {
	principal := billing.PrincipalForUser(userID)
	decision, err := entitlements.CheckPrincipalFeature(r.Context(),
		entitlements.Deps{Pool: h.d.Pool}, principal, entitlements.FeatureInboxRules)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notif rules: entitlement check", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return false
	}
	if !decision.Allowed {
		mode := "report_only"
		if h.d.BillingEnforce.UserInboxRules {
			mode = "enforce"
		}
		h.d.Logger.InfoContext(r.Context(), "entitlements.report_only_deny",
			"principal", principal.String(),
			"principal_kind", string(principal.Kind),
			"principal_id", userID,
			"feature", string(entitlements.FeatureInboxRules),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "settings-inbox-rules",
			"action", action)
		if h.d.BillingEnforce.UserInboxRules {
			http.Error(w,
				"notification routing rules require a Pro subscription — see /settings/billing",
				http.StatusPaymentRequired)
			return false
		}
	}
	return true
}

// parseRuleID extracts the {id} chi URL param. 404 on garbage so
// attackers can't distinguish bad-shape from not-yours.
func parseRuleID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return 0, false
	}
	return id, true
}
