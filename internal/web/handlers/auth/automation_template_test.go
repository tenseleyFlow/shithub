// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

// PRO-EXT01-13c: production HTML render tests for the automation
// settings page. Mirrors the PRO-EXT_SR-07 pattern — render the
// real template (not a stub) so XSS regressions in destination URLs,
// cron expressions, or workflow paths are caught at the template
// boundary.

import (
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/cronworkflow"
	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
)

func automationData(allowed bool, relays []webhookrelay.Relay, cron []cronworkflow.Dispatch) map[string]any {
	return merge(baseLayoutFields(), map[string]any{
		"Title":          "Automation",
		"SettingsActive": "automation",
		"Allowed":        allowed,
		"FineGrainedKey": "webhook_relay",
		"Relays":         relays,
		"CronDispatches": cron,
		"BaseURL":        "https://shithub.test",
	})
}

func TestAutomationTemplate_RendersWithoutError(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/automation",
		automationData(true,
			[]webhookrelay.Relay{
				{
					ID: 1, Name: "github-mirror", TokenPrefix: "shrelay_abcd",
					Destinations: []webhookrelay.Destination{{URL: "https://dest.example.test/"}},
				},
			},
			[]cronworkflow.Dispatch{
				{
					ID: 1, WorkflowFile: ".shithub/workflows/ci.yml",
					Ref: "refs/heads/trunk", CronExpr: "0 * * * *",
					NextFireAt:     time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC),
					LastFireStatus: "fired",
				},
			}))
	assertContains(t, body, `<h1>Automation</h1>`, "page heading")
	assertContains(t, body, `github-mirror`, "relay name row")
	assertContains(t, body, `shrelay_abcd`, "relay token prefix")
	assertContains(t, body, `0 * * * *`, "cron expression row")
}

func TestAutomationTemplate_EscapesDestinationURLXSS(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/automation",
		automationData(true,
			[]webhookrelay.Relay{
				{
					ID: 1, Name: "XSS_" + xssPayload, TokenPrefix: "shrelay_aaaa",
					Destinations: []webhookrelay.Destination{{URL: xssPayload}},
				},
			},
			nil))
	assertNotContains(t, body, xssPayload,
		"raw <script> payload leaked through destination URL or relay name")
	assertContains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;",
		"escaped payload — value should appear escaped, not omitted")
}

func TestAutomationTemplate_EscapesCronExprXSS(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/automation",
		automationData(true, nil,
			[]cronworkflow.Dispatch{
				{
					ID: 1, WorkflowFile: ".shithub/" + xssPayload,
					Ref: "refs/heads/trunk", CronExpr: xssPayload,
					NextFireAt:     time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC),
					LastFireStatus: "pending",
				},
			}))
	assertNotContains(t, body, xssPayload,
		"raw <script> payload leaked through cron_expr or workflow_file")
	assertContains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;",
		"escaped payload should appear")
}

func TestAutomationTemplate_FreeUserSeesDisabledForms(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/automation",
		automationData(false, nil, nil))

	assertContains(t, body, `shithub-pro-lock`, "Pro-lock wrapper")
	assertContains(t, body, `data-pro-feature="webhook_relay"`,
		"feature slug threaded into the lock wrapper")
	assertContains(t, body, `disabled aria-disabled="true"`,
		"form inputs should be disabled for Free users")
	assertContains(t, body, `href="/settings/billing"`, "upgrade CTA link")
}
