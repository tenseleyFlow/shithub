// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

// PRO-EXT01-16b: production HTML render test for the digest section
// of /settings/notifications.

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
)

func digestData(digestAllowed bool, hasDigest bool, digest notifdb.UserNotificationDigest) map[string]any {
	return merge(baseLayoutFields(), map[string]any{
		"Title":              "Notifications",
		"SettingsActive":     "notifications",
		"Channels":           []struct{}{},
		"Success":            "",
		"Rules":              []notifdb.UserNotificationRule{},
		"RulesAllowed":       digestAllowed, // same allowed state so rules section doesn't muddy the assertion
		"RulesFeatureKey":    "inbox_rules",
		"RuleActionSnooze":   "snooze",
		"RuleActionTab":      "tab",
		"RuleActionMarkRead": "mark_read",
		"RuleActionDrop":     "drop",
		"Digest":             digest,
		"HasDigest":          hasDigest,
		"DigestAllowed":      digestAllowed,
		"DigestFeatureKey":   "inbox_digests",
		"DigestFreqDaily":    "daily",
		"DigestFreqWeekly":   "weekly",
	})
}

func TestDigestTemplate_EnabledForPro(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/notifications", digestData(true, false,
		notifdb.UserNotificationDigest{HourUtc: 9, Frequency: notifdb.UserNotificationDigestFrequencyDaily}))
	assertContains(t, body, `Digest email`, "section heading")
	assertContains(t, body, `action="/settings/notifications/digest"`, "save form action")
	assertContains(t, body, `name="frequency"`, "frequency select")
	assertContains(t, body, `name="hour_utc"`, "hour input")
}

func TestDigestTemplate_LockedForFree(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/notifications", digestData(false, false,
		notifdb.UserNotificationDigest{HourUtc: 9, Frequency: notifdb.UserNotificationDigestFrequencyDaily}))
	assertContains(t, body, `data-pro-feature="inbox_digests"`, "feature key on digest lock wrapper")
	assertContains(t, body, `disabled aria-disabled="true"`, "digest form inputs disabled")
}

func TestDigestTemplate_EnabledScheduleShowsDisableButton(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	row := notifdb.UserNotificationDigest{
		Enabled:    true,
		HourUtc:    14,
		Frequency:  notifdb.UserNotificationDigestFrequencyWeekly,
		DayOfWeek:  pgtype.Int2{Int16: 2, Valid: true},
		NextSendAt: pgtype.Timestamptz{Time: time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC), Valid: true},
	}
	body := renderPage(t, rr, "settings/notifications", digestData(true, true, row))
	assertContains(t, body, `Digest is`, "status banner present when enabled")
	assertContains(t, body, `action="/settings/notifications/digest/disable"`, "disable form action")
	assertContains(t, body, `weekly`, "frequency text rendered")
}
