// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

// PRO-EXT_SR-07: production HTML render tests for the settings pages
// touched by PRO-EXT01-12 (and its 11-series dependencies). The
// handler-level tests in this package substitute a stub template that
// emits SECRETS=NAME; / VARS=NAME=value; markers — useful for handler
// wiring, useless for catching XSS regressions in the real template.
//
// These tests render the actual production template (loaded via
// web.TemplatesFS()) so that:
//   - a value swap to `template.HTML` or `safeHTML` would be caught;
//   - a new field inlined without escaping would be caught;
//   - the Free-locked variant continues to render disabled controls.
//
// The tests bypass the HTTP/DB layer entirely — they call the renderer
// directly with hand-built data shapes matching what the handlers pass.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	"github.com/tenseleyFlow/shithub/internal/actions/variables"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func newProductionRenderer(t *testing.T) *render.Renderer {
	t.Helper()
	rr, err := render.New(web.TemplatesFS(), render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		t.Fatalf("render.New(web.TemplatesFS): %v", err)
	}
	return rr
}

// baseLayoutFields are the keys the shared layout/nav/footer partials
// consult on every page. Tests merge feature-specific keys over these.
func baseLayoutFields() map[string]any {
	return map[string]any{
		"Title":     "test page",
		"Viewer":    middleware.CurrentUser{},
		"CSRFToken": "test-csrf-token",
	}
}

func merge(base map[string]any, overrides map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func renderPage(t *testing.T, rr *render.Renderer, name string, data map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := rr.Render(&buf, name, data); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return buf.String()
}

// xssPayload is the canonical "did you forget to escape" probe.
// Any production template that html/template lets through unchanged
// would fail these assertions.
const xssPayload = `<script>alert(1)</script>`

func assertNotContains(t *testing.T, body, needle, why string) {
	t.Helper()
	if strings.Contains(body, needle) {
		t.Fatalf("body contains %q (should not — %s)\nfirst 400 chars: %s",
			needle, why, truncate(body, 400))
	}
}

func assertContains(t *testing.T, body, needle, why string) {
	t.Helper()
	if !strings.Contains(body, needle) {
		t.Fatalf("body missing %q (%s)\nfirst 1200 chars: %s",
			needle, why, truncate(body, 1200))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --- settings/actions_secrets.html -----------------------------------

func actionsSecretsData(allowed bool, secretName, varName, varValue string) map[string]any {
	now := pgtype.Timestamptz{Time: time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC), Valid: true}
	return merge(baseLayoutFields(), map[string]any{
		"Title":          "Personal Actions secrets",
		"SettingsActive": "actions-secrets",
		"Secrets": []secrets.Meta{
			{ID: 1, Name: secretName, UpdatedAt: now},
		},
		"Variables": []variables.Variable{
			{ID: 1, Name: varName, Value: varValue, UpdatedAt: now},
		},
		"Allowed":        allowed,
		"FineGrainedKey": "user.actions_secrets",
		"CreateError":    "",
		"SecretFormName": "",
		"VarFormName":    "",
	})
}

func TestActionsSecretsTemplate_RendersWithoutError(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/actions_secrets",
		actionsSecretsData(true, "DEPLOY_KEY", "REGISTRY_HOST", "ghcr.io"))
	assertContains(t, body, `<h1>Personal Actions secrets</h1>`, "page heading")
	assertContains(t, body, `DEPLOY_KEY`, "secret name row")
	assertContains(t, body, `REGISTRY_HOST`, "variable name row")
	assertContains(t, body, `ghcr.io`, "variable value row")
}

func TestActionsSecretsTemplate_EscapesVariableValueXSS(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	// Pro user — same render path, but verifies the value cell escapes
	// rather than blindly emits user input. A regression that swaps
	// `{{ .Value }}` to `{{ .Value | safeHTML }}` would fail here.
	body := renderPage(t, rr, "settings/actions_secrets",
		actionsSecretsData(true, "ALSO_"+xssPayload, "VAR_"+xssPayload, xssPayload))

	// The literal payload must not appear verbatim — html/template
	// must have escaped the angle brackets.
	assertNotContains(t, body, xssPayload, "raw <script> payload leaked through value cell")
	assertNotContains(t, body, "<script>alert(1)</script>", "raw script tag rendered")

	// The escaped form should be present — proves the value is on the
	// page (so we're not just passing because the field went missing)
	// in its safe form.
	assertContains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;",
		"escaped payload — value should appear escaped, not omitted")
}

func TestActionsSecretsTemplate_FreeUserSeesDisabledForm(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/actions_secrets",
		actionsSecretsData(false, "EXISTING_SECRET", "EXISTING_VAR", "value"))

	assertContains(t, body, `shithub-pro-lock`, "Pro-lock wrapper class for the gated banner")
	assertContains(t, body, `data-pro-feature="user.actions_secrets"`,
		"feature slug threaded into the lock wrapper for conversion telemetry")
	assertContains(t, body, `disabled aria-disabled="true"`,
		"form inputs should be disabled for Free users")
	// CTA goes to billing — without this, the lock surface has no
	// upgrade path.
	assertContains(t, body, `href="/settings/billing"`, "upgrade CTA link")
}

// --- settings/tokens.html --------------------------------------------

func tokensData(fineGrainedAllowed bool) map[string]any {
	expires := pgtype.Timestamptz{Time: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	return merge(baseLayoutFields(), map[string]any{
		"Title":          "Personal access tokens",
		"SettingsActive": "tokens",
		"Tokens": []usersdb.UserToken{
			{
				ID: 1, UserID: 1, Name: "ci-runner", TokenPrefix: "shp_abcd",
				Scopes: []string{"repo:read"}, ExpiresAt: expires,
			},
		},
		"AllScopes":          []string{"repo:read", "repo:write"},
		"CreateError":        "",
		"CreateName":         "",
		"CreateScopes":       []string{},
		"JustCreatedRaw":     "",
		"RecentAuthOK":       true,
		"FineGrainedAllowed": fineGrainedAllowed,
		"FineGrainedKey":     "user.fine_grained_pats",
		"RepoChoices":        []struct{}{}, // empty slice — only the Pro branch ranges it
	})
}

func TestTokensTemplate_RendersWithoutError(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/tokens", tokensData(true))
	assertContains(t, body, `<h1>Personal access tokens</h1>`, "page heading")
	assertContains(t, body, `ci-runner`, "token name should appear")
	assertContains(t, body, `shp_abcd`, "token prefix should appear")
}

func TestTokensTemplate_EscapesTokenNameXSS(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	data := tokensData(true)
	expires := pgtype.Timestamptz{Time: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	data["Tokens"] = []usersdb.UserToken{
		{
			ID: 1, UserID: 1, Name: xssPayload, TokenPrefix: "shp_abcd",
			Scopes: []string{"repo:read"}, ExpiresAt: expires,
		},
	}
	// JustCreatedRaw is rendered inside <pre><code> right after creation;
	// it's user-derived (the raw token string) so it must escape too.
	data["JustCreatedRaw"] = xssPayload

	body := renderPage(t, rr, "settings/tokens", data)
	assertNotContains(t, body, "<script>alert(1)</script>",
		"unescaped XSS payload in token name or just-created raw")
	assertContains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;",
		"escaped payload should appear in place of the raw form")
}

func TestTokensTemplate_FreeUserSeesDisabledFineGrainedControls(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/tokens", tokensData(false))

	// The IP allowlist + repo binding fields are wrapped in
	// shithub-pro-lock when FineGrainedAllowed is false.
	assertContains(t, body, `shithub-pro-lock`, "Pro-lock wrapper for the gated form rows")
	assertContains(t, body, `data-pro-feature="user.fine_grained_pats"`,
		"feature slug threaded into the lock wrapper")
	assertContains(t, body, `disabled aria-disabled="true"`,
		"Free users should see disabled controls")
	assertContains(t, body, `href="/settings/billing"`, "upgrade CTA link")
}

// --- settings/token_analytics.html -----------------------------------

type dayCountView struct {
	Day   string
	Count int64
}

type topRouteView struct {
	Method      string
	RoutePrefix string
	EventCount  int64
}

func tokenAnalyticsData(isPreview bool) map[string]any {
	expires := pgtype.Timestamptz{Time: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	tok := usersdb.UserToken{
		ID: 1, UserID: 1, Name: "ci-runner", TokenPrefix: "shp_abcd",
		Scopes: []string{"repo:read"}, ExpiresAt: expires,
	}
	data := merge(baseLayoutFields(), map[string]any{
		"Title":          "Token analytics",
		"SettingsActive": "tokens",
		"Token":          tok,
		"Allowed":        !isPreview,
		"FineGrainedKey": "user.fine_grained_pats",
		"TotalRequests":  int64(42),
		"DailyCounts": []dayCountView{
			{Day: "2026-05-14", Count: 10},
			{Day: "2026-05-15", Count: 32},
		},
		"TopRoutes": []topRouteView{
			{Method: "GET", RoutePrefix: "/api/v1/repos", EventCount: 20},
		},
		"IsPreview":     isPreview,
		"UpgradeBanner": "Upgrade to Pro to see real analytics data.",
	})
	return data
}

func TestTokenAnalyticsTemplate_RendersWithoutError(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/token_analytics", tokenAnalyticsData(false))
	assertContains(t, body, `Analytics — ci-runner`, "page heading with token name")
	assertContains(t, body, `Total requests:</strong> 42`, "total request count")
	assertContains(t, body, `/api/v1/repos`, "top route should render")
}

func TestTokenAnalyticsTemplate_EscapesRoutePrefixXSS(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	data := tokenAnalyticsData(false)
	data["TopRoutes"] = []topRouteView{
		{Method: "GET", RoutePrefix: xssPayload, EventCount: 1},
	}
	// Token name also user-controlled — verify it escapes too.
	expires := pgtype.Timestamptz{Time: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	data["Token"] = usersdb.UserToken{
		ID: 1, UserID: 1, Name: xssPayload, TokenPrefix: "shp_abcd",
		Scopes: []string{"repo:read"}, ExpiresAt: expires,
	}

	body := renderPage(t, rr, "settings/token_analytics", data)
	assertNotContains(t, body, "<script>alert(1)</script>",
		"unescaped XSS in route prefix or token name")
	assertContains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;",
		"escaped payload should be present")
}

func TestTokenAnalyticsTemplate_FreeUserSeesPreviewLock(t *testing.T) {
	t.Parallel()
	rr := newProductionRenderer(t)
	body := renderPage(t, rr, "settings/token_analytics", tokenAnalyticsData(true))

	assertContains(t, body, `shithub-pro-lock`, "Pro-lock wrapper around the preview banner")
	assertContains(t, body, `data-pro-feature="user.fine_grained_pats"`,
		"feature slug for conversion telemetry")
	assertContains(t, body, `<em>(sample)</em>`,
		"preview marker beside the total count")
	assertContains(t, body, `Upgrade to Pro`,
		"upgrade banner message should render")
	assertContains(t, body, `href="/settings/billing"`, "upgrade CTA link")
}
