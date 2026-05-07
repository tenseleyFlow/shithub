// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"encoding/json"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
)

// kindToChannel maps a NoticeMessage `kind` to the user-toggleable
// notification channel that gates it. Kinds whose channel is
// "security_alerts" are required and never suppressed (the user can't
// opt out of having their account told about a security event). Kinds
// whose channel is "account_changes" honor the user's preference.
var kindToChannel = map[string]string{
	// security_alerts (always-on).
	"2fa_enabled":          "security_alerts",
	"2fa_disabled":         "security_alerts",
	"recovery_regenerated": "security_alerts",
	"admin_cleared_2fa":    "security_alerts",
	"password_changed":     "security_alerts",
	"log_out_everywhere":   "security_alerts",
	// account_changes (opt-out).
	"username_changed":           "account_changes",
	"primary_email_changed":      "account_changes",
	"account_deletion_initiated": "account_changes",
}

// notifyState sends a notification email about a state change. Best
// effort — failure is logged but does not break the flow. Honors the
// user's notification prefs: security_alerts kinds are always sent,
// account_changes kinds are skipped silently when the user has opted out.
func (h *Handlers) notifyState(ctx context.Context, userID int64, kind string) {
	if !h.shouldNotify(ctx, userID, kind) {
		return
	}
	user, err := h.q.GetUserByID(ctx, h.d.Pool, userID)
	if err != nil || !user.PrimaryEmailID.Valid {
		return
	}
	em, err := h.q.GetUserEmailByID(ctx, h.d.Pool, user.PrimaryEmailID.Int64)
	if err != nil {
		return
	}
	msg, err := email.NoticeMessage(h.d.Branding, string(em.Email), user.Username, kind)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "notify: build", "kind", kind, "error", err)
		return
	}
	if err := h.d.Email.Send(ctx, msg); err != nil {
		h.d.Logger.WarnContext(ctx, "notify: send", "kind", kind, "error", err)
	}
}

// shouldNotify checks the user's persisted prefs for the channel
// associated with kind. Returns true (allow) on any uncertainty so a DB
// blip doesn't suppress a security email.
func (h *Handlers) shouldNotify(ctx context.Context, userID int64, kind string) bool {
	channel, ok := kindToChannel[kind]
	if !ok {
		return true
	}
	if channel == "security_alerts" {
		return true
	}
	rows, err := h.q.ListUserNotificationPrefs(ctx, h.d.Pool, userID)
	if err != nil {
		return true
	}
	for _, p := range rows {
		if p.Key != channel {
			continue
		}
		var v notifPrefValue
		if err := json.Unmarshal(p.Value, &v); err != nil {
			return true
		}
		return v.Enabled
	}
	// No persisted row → use the channel default.
	return notifChannelDefault(channel)
}
