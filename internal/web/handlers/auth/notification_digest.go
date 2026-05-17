// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	"github.com/tenseleyFlow/shithub/internal/notifications"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-16b: digest schedule settings on /settings/notifications.
// Two routes:
//
//	POST /settings/notifications/digest          — save schedule
//	POST /settings/notifications/digest/disable  — pause digest
//
// Disable is a separate POST (rather than checkbox in the save form)
// so the operator can document "click here to stop digests" without
// the form-state ambiguity. It's NOT entitlement-gated — a Pro→Free
// downgrade must still let the user turn the schedule off.

func (h *Handlers) settingsDigestSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	if !h.inboxDigestGate(w, r, user.ID, "save") {
		return
	}

	freq := notifdb.UserNotificationDigestFrequency(r.PostFormValue("frequency"))
	switch freq {
	case notifdb.UserNotificationDigestFrequencyDaily,
		notifdb.UserNotificationDigestFrequencyWeekly:
	default:
		http.Error(w, "invalid frequency (daily|weekly)", http.StatusBadRequest)
		return
	}

	hour, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("hour_utc")))
	if err != nil || hour < 0 || hour > 23 {
		http.Error(w, "hour_utc must be 0-23", http.StatusBadRequest)
		return
	}

	var dow pgtype.Int2
	if freq == notifdb.UserNotificationDigestFrequencyWeekly {
		raw := strings.TrimSpace(r.PostFormValue("day_of_week"))
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 || v > 6 {
			http.Error(w, "day_of_week must be 0-6 for weekly", http.StatusBadRequest)
			return
		}
		dow = pgtype.Int2{Int16: int16(v), Valid: true}
	}

	next := notifications.NextSendTime(time.Now().UTC(), freq, hour, intValueOrZero(dow))
	if _, err := notifdb.New().UpsertUserNotificationDigest(r.Context(), h.d.Pool, notifdb.UpsertUserNotificationDigestParams{
		UserID:     user.ID,
		Enabled:    true,
		Frequency:  freq,
		HourUtc:    int16(hour),
		DayOfWeek:  dow,
		NextSendAt: pgtype.Timestamptz{Time: next, Valid: true},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notif digest: upsert", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/notifications", http.StatusSeeOther)
}

func (h *Handlers) settingsDigestDisable(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	row, err := notifdb.New().GetUserNotificationDigest(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Nothing to disable; treat as success so the user can't
			// distinguish "never enabled" from "just disabled".
			http.Redirect(w, r, "/settings/notifications", http.StatusSeeOther)
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "notif digest: get", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if _, err := notifdb.New().UpsertUserNotificationDigest(r.Context(), h.d.Pool, notifdb.UpsertUserNotificationDigestParams{
		UserID:     row.UserID,
		Enabled:    false,
		Frequency:  row.Frequency,
		HourUtc:    row.HourUtc,
		DayOfWeek:  row.DayOfWeek,
		NextSendAt: pgtype.Timestamptz{}, // clear next_send_at so the sweep skips
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notif digest: disable", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/notifications", http.StatusSeeOther)
}

// inboxDigestGate enforces FeatureInboxDigests at the save site.
func (h *Handlers) inboxDigestGate(w http.ResponseWriter, r *http.Request, userID int64, action string) bool {
	principal := billing.PrincipalForUser(userID)
	decision, err := entitlements.CheckPrincipalFeature(r.Context(),
		entitlements.Deps{Pool: h.d.Pool}, principal, entitlements.FeatureInboxDigests)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notif digest: entitlement check", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return false
	}
	if !decision.Allowed {
		mode := "report_only"
		if h.d.BillingEnforce.UserInboxDigests {
			mode = "enforce"
		}
		h.d.Logger.InfoContext(r.Context(), "entitlements.report_only_deny",
			"principal", principal.String(),
			"principal_kind", string(principal.Kind),
			"principal_id", userID,
			"feature", string(entitlements.FeatureInboxDigests),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "settings-inbox-digest",
			"action", action)
		if h.d.BillingEnforce.UserInboxDigests {
			http.Error(w,
				"notification digests require a Pro subscription — see /settings/billing",
				http.StatusPaymentRequired)
			return false
		}
	}
	return true
}

func intValueOrZero(v pgtype.Int2) int {
	if !v.Valid {
		return 0
	}
	return int(v.Int16)
}
