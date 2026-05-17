// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

// PRO-EXT01 wrap follow-up: production HTML render test for the
// compare-plans matrix on /settings/billing. The table is a long
// static list of features; without a pinning test, a future template
// edit can silently drop a row and we lose the marketing surface
// without noticing. Mirrors the PRO-EXT_SR-07 pattern.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/web"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func newBillingRenderer(t *testing.T) *render.Renderer {
	t.Helper()
	rr, err := render.New(web.TemplatesFS(), render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	return rr
}

func billingPageData() map[string]any {
	//nolint:gosec // fake test CSRF token.
	const fakeCSRF = "test-csrf-token"
	return map[string]any{
		"Title":                 "Billing and plans",
		"Viewer":                middleware.CurrentUser{},
		"CSRFToken":             fakeCSRF,
		"SettingsActive":        "billing",
		"Summary":               []any{},
		"CanManageSubscription": false,
		"CanStartCheckout":      true,
		"GracePeriodLabel":      "14 days",
		"Invoices":              []any{},
		"IsSiteAdmin":           false,
	}
}

func renderBilling(t *testing.T, rr *render.Renderer, data map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := rr.Render(&buf, "settings/billing", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// TestBillingCompareTable_ListsEveryProFeature pins the campaign-shipped
// feature inventory on the Compare plans table. A feature dropped from
// this list is invisible to a prospective Pro customer — which is the
// whole point of the surface. Add to this list any time PRO-EXT01-NN
// (or a follow-up campaign) lands a new gated user-tier feature.
func TestBillingCompareTable_ListsEveryProFeature(t *testing.T) {
	t.Parallel()
	rr := newBillingRenderer(t)
	body := renderBilling(t, rr, billingPageData())

	// Section headings the matrix groups features under. Keep these
	// in sync with billing.html's <th scope="rowgroup"> rows.
	for _, section := range []string{
		"Core", "Identity &amp; profile", "Productivity",
		"Privacy &amp; security", "Automation",
		"Discoverability &amp; lifecycle", "Notifications",
	} {
		if !strings.Contains(body, section) {
			t.Errorf("compare table missing section heading %q", section)
		}
	}

	// One assertion per feature row. The substring is a stable
	// fragment of the human-readable row label — short enough that
	// a copy edit on the surrounding wording doesn't break the test,
	// long enough that no other row collides with it.
	for _, want := range []string{
		"Unlimited public and private repositories",
		"Pinned repositories on profile",
		"Profile accent color",
		"Animated GIF avatars",
		"Reserve previous usernames",
		"Pro badge across profile",
		"Saved replies for issues and PRs",
		"Schedule issues to file later",
		"Private repositories as templates",
		"Regex code search",
		"Advanced branch protection on private repos",
		"Required reviewers on private repos",
		"Per-repo contribution-graph privacy",
		"Historical secret scanning",
		"Secret-scan alerts",
		"Fine-grained PATs",
		"Actions secrets &amp; variables (user scope",
		"Personal webhook relay",
		"Cron-scheduled workflow dispatch",
		"Personal status page",
		"Repository pause",
		"Focus-mode inbox",
		"Scheduled daily/weekly digest emails",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("compare table missing row %q", want)
		}
	}
}

// TestBillingCompareTable_VisualMarkersPresent confirms the
// checkmark/dash icon scheme actually renders. A regression that
// drops the octicon SVG (e.g. by renaming the helper) shows a blank
// cell on a public-facing page.
func TestBillingCompareTable_VisualMarkersPresent(t *testing.T) {
	t.Parallel()
	rr := newBillingRenderer(t)
	body := renderBilling(t, rr, billingPageData())

	if !strings.Contains(body, `shithub-plan-yes`) {
		t.Errorf("missing yes-marker class")
	}
	if !strings.Contains(body, `shithub-plan-no`) {
		t.Errorf("missing no-marker class")
	}
	// Pro column gets a highlight class so the prospect's eye lands
	// in the right column.
	if !strings.Contains(body, `shithub-plan-col-pro`) {
		t.Errorf("missing Pro column highlight class")
	}
}
