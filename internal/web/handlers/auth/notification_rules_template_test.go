// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

// PRO-EXT01-16a: production HTML render test for the routing-rules
// section of /settings/notifications. Mirrors PRO-EXT_SR-07 pattern.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
)

func notificationsData(rulesAllowed bool, rules []notifdb.UserNotificationRule) map[string]any {
	return merge(baseLayoutFields(), map[string]any{
		"Title":          "Notifications",
		"SettingsActive": "notifications",
		"Channels":       []struct{}{},
		"Success":        "",
		"Rules":          rules,
		"RulesAllowed":   rulesAllowed,
		// Feature key + action enum strings must match what the
		// production handler passes; if a refactor renames either,
		// the template will print blank labels and this test will
		// catch it.
		"RulesFeatureKey":    "inbox_rules",
		"RuleActionSnooze":   string(notifdb.UserNotificationRuleActionSnooze),
		"RuleActionTab":      string(notifdb.UserNotificationRuleActionTab),
		"RuleActionMarkRead": string(notifdb.UserNotificationRuleActionMarkRead),
		"RuleActionDrop":     string(notifdb.UserNotificationRuleActionDrop),
	})
}

func TestNotificationsTemplate_RulesEnabledForPro(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	rules := []notifdb.UserNotificationRule{
		{
			ID: 1, Name: "security alerts", Enabled: true,
			MatchReason: pgtype.Text{String: "security_alert", Valid: true},
			Action:      notifdb.UserNotificationRuleActionTab,
			ActionTab:   pgtype.Text{String: "security", Valid: true},
		},
	}
	body := renderPage(t, rr, "settings/notifications", notificationsData(true, rules))
	assertContains(t, body, `Routing rules`, "section heading")
	assertContains(t, body, `security alerts`, "existing rule row name")
	assertContains(t, body, `security_alert`, "match reason shown in row")
	assertContains(t, body, `action="/settings/notifications/rules"`, "add-rule form action")
	if found := contains(body, `disabled aria-disabled="true"`); found {
		t.Errorf("Pro user should not see disabled controls in rules section")
	}
}

func TestNotificationsTemplate_RulesLockedForFree(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/notifications", notificationsData(false, nil))
	assertContains(t, body, `Routing rules`, "section heading")
	assertContains(t, body, `shithub-pro-lock`, "Pro-lock wrapper on add-rule form")
	assertContains(t, body, `data-pro-feature="inbox_rules"`, "feature slug on lock wrapper")
	assertContains(t, body, `disabled aria-disabled="true"`, "form inputs disabled for Free")
	assertContains(t, body, `href="/settings/billing"`, "upgrade CTA link")
}

func TestNotificationsTemplate_RuleActionSelectOptions(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/notifications", notificationsData(true, nil))
	// All four action options must be present in the select. A
	// regression that drops one (e.g. by typo in the template) would
	// silently strip the affordance.
	assertContains(t, body, `value="snooze"`, "snooze action option")
	assertContains(t, body, `value="tab"`, "tab action option")
	assertContains(t, body, `value="mark_read"`, "mark_read action option")
	assertContains(t, body, `value="drop"`, "drop action option")
}

// contains is a non-fatal substring check; assertContains exits on
// miss, contains lets a test write its own "must NOT contain" check.
func contains(body, needle string) bool {
	return len(body) > 0 && stringIndex(body, needle) >= 0
}

func stringIndex(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
