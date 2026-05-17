// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

// PRO-EXT01-10d: production HTML render test for the secret-scan
// alerts settings page. Mirrors PRO-EXT_SR-07 — render the real
// template (web.TemplatesFS()) so a regression that drops the
// pro-lock wrapper for Free users is caught at the template boundary.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/web"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func newSSARenderer(t *testing.T) *render.Renderer {
	t.Helper()
	rr, err := render.New(web.TemplatesFS(), render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	return rr
}

func ssaData(allowed bool, emailEnabled bool, webhookURL string) map[string]any {
	//nolint:gosec // fake test CSRF token.
	const fakeCSRF = "test-csrf-token"
	return map[string]any{
		"Title":          "Secret scan alerts",
		"Viewer":         middleware.CurrentUser{},
		"CSRFToken":      fakeCSRF,
		"SettingsActive": "secret-scan-alerts",
		"AlertsAllowed":  allowed,
		"FeatureKey":     "secret_scan_alerts",
		"Form": map[string]any{
			"EmailEnabled":    emailEnabled,
			"WebhookURL":      webhookURL,
			"HasWebhookSet":   webhookURL != "",
			"WebhookRotation": "",
		},
	}
}

func renderSSA(t *testing.T, rr *render.Renderer, data map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := rr.Render(&buf, "settings/secret_scan_alerts", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestSSATemplate_ProUserSeesForm(t *testing.T) {
	t.Parallel()
	rr := newSSARenderer(t)
	body := renderSSA(t, rr, ssaData(true, false, ""))
	if !strings.Contains(body, `action="/settings/secret-scanning/alerts"`) {
		t.Errorf("missing save form: %s", body)
	}
	if !strings.Contains(body, `name="email_enabled"`) {
		t.Errorf("missing email_enabled input")
	}
	if strings.Contains(body, `shithub-pro-lock`) {
		t.Errorf("Pro user should NOT see pro-lock wrapper")
	}
}

func TestSSATemplate_FreeUserLocked(t *testing.T) {
	t.Parallel()
	rr := newSSARenderer(t)
	body := renderSSA(t, rr, ssaData(false, false, ""))
	if !strings.Contains(body, `shithub-pro-lock`) {
		t.Errorf("Free user must see pro-lock wrapper: %s", body)
	}
	if !strings.Contains(body, `data-pro-feature="secret_scan_alerts"`) {
		t.Errorf("missing feature key data-attr")
	}
	if !strings.Contains(body, `href="/settings/billing"`) {
		t.Errorf("missing upgrade CTA")
	}
	if !strings.Contains(body, `disabled aria-disabled="true"`) {
		t.Errorf("Free user form must show disabled controls")
	}
}

func TestSSATemplate_WebhookRotationDisplayedOnce(t *testing.T) {
	t.Parallel()
	rr := newSSARenderer(t)
	data := ssaData(true, false, "https://example.com/hook")
	data["Form"].(map[string]any)["WebhookRotation"] = "deadbeefcafef00d"
	body := renderSSA(t, rr, data)
	if !strings.Contains(body, "deadbeefcafef00d") {
		t.Errorf("rotation secret not displayed: %s", body)
	}
	if !strings.Contains(body, "won't be shown again") {
		t.Errorf("missing one-time-display warning")
	}
}
