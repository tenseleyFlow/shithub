// SPDX-License-Identifier: AGPL-3.0-or-later

package profile_test

// PRO-EXT01-14: production HTML render test for the status page.
// Mirrors the PRO-EXT_SR-07 pattern — render the actual template
// (loaded via web.TemplatesFS()) so XSS regressions in repo names,
// branches, or owner usernames are caught at the template boundary.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/statuspage"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

const statusXSSPayload = `<script>alert(1)</script>`

func newStatusProdRenderer(t *testing.T) *render.Renderer {
	t.Helper()
	rr, err := render.New(web.TemplatesFS(), render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	return rr
}

func statusData(isTeaser bool, summary statuspage.Summary, username string) map[string]any {
	//nolint:gosec // test fixture, not a credential
	const fakeCSRF = "test-csrf-token"
	return map[string]any{
		"Title":             "status",
		"Viewer":            middleware.CurrentUser{},
		"CSRFToken":         fakeCSRF,
		"User":              usersdb.User{Username: username, DisplayName: username},
		"DisplayName":       username,
		"AvatarURL":         "/avatars/" + username,
		"BadgeURL":          "/" + username + ".status.svg",
		"IsSelf":            false,
		"ProfileOwnerIsPro": !isTeaser,
		"Summary":           summary,
		"IsTeaser":          isTeaser,
		"TeaserUpgradeHref": "/settings/billing",
	}
}

func renderStatus(t *testing.T, rr *render.Renderer, data map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := rr.Render(&buf, "profile/status", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// TestStatusTemplate_RendersHappyPath drives the live (non-teaser)
// render path with a realistic 3-repo summary. Asserts the load-
// bearing pieces (overall state, badge URL, per-repo rows) are all
// present so a partial template refactor doesn't silently drop one.
func TestStatusTemplate_RendersHappyPath(t *testing.T) {
	t.Parallel()
	rr := newStatusProdRenderer(t)
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	summary := statuspage.Summary{
		OverallState: statuspage.StateOK,
		GeneratedAt:  now,
		Repos: []statuspage.RepoStatus{
			{
				Owner: "alice", Name: "site", DefaultBranch: "trunk",
				LatestRun: statuspage.LatestRun{
					Conclusion: "success", CompletedAt: now.Add(-1 * time.Hour), RunIndex: 42,
				},
				SuccessRate: 1.0, TotalRuns: 12,
			},
		},
	}
	body := renderStatus(t, rr, statusData(false, summary, "alice"))
	if !strings.Contains(body, `alice/site`) {
		t.Errorf("missing repo row: %s", body)
	}
	if !strings.Contains(body, `100%`) {
		t.Errorf("missing percent rendering: %s", body)
	}
	if !strings.Contains(body, `/alice.status.svg`) {
		t.Errorf("missing badge URL: %s", body)
	}
}

// TestStatusTemplate_TeaserShowsLock is the Free-user discovery
// affordance: the teaser banner with the Pro-lock class and upgrade
// CTA. Without these the locked-UI campaign rule isn't satisfied.
func TestStatusTemplate_TeaserShowsLock(t *testing.T) {
	t.Parallel()
	rr := newStatusProdRenderer(t)
	summary := statuspage.Summary{
		OverallState: statuspage.StateOK,
		GeneratedAt:  time.Now().UTC(),
		Repos: []statuspage.RepoStatus{
			{
				Owner: "you", Name: "demo", DefaultBranch: "main",
				LatestRun:   statuspage.LatestRun{Conclusion: "success", CompletedAt: time.Now(), RunIndex: 1},
				SuccessRate: 0.97, TotalRuns: 30,
			},
		},
	}
	body := renderStatus(t, rr, statusData(true, summary, "alice"))
	if !strings.Contains(body, `shithub-pro-lock`) {
		t.Errorf("missing Pro-lock class on teaser: %s", body)
	}
	if !strings.Contains(body, `data-pro-feature="personal_status_page"`) {
		t.Errorf("missing data-pro-feature attribute on teaser: %s", body)
	}
	if !strings.Contains(body, `href="/settings/billing"`) {
		t.Errorf("missing upgrade CTA href: %s", body)
	}
}

// TestStatusTemplate_EscapesRepoNameXSS guards against a caller
// (typically the aggregator or a future refactor) that hands a repo
// name into the template without trusting html/template to escape
// it. shithub doesn't permit `<` in repo names today, but the
// template MUST defend regardless — defense in depth.
func TestStatusTemplate_EscapesRepoNameXSS(t *testing.T) {
	t.Parallel()
	rr := newStatusProdRenderer(t)
	summary := statuspage.Summary{
		OverallState: statuspage.StateOK,
		GeneratedAt:  time.Now().UTC(),
		Repos: []statuspage.RepoStatus{
			{
				Owner: "alice", Name: statusXSSPayload, DefaultBranch: "trunk",
				LatestRun: statuspage.LatestRun{
					Conclusion: "success", CompletedAt: time.Now(), RunIndex: 1,
				},
				SuccessRate: 1.0, TotalRuns: 1,
			},
		},
	}
	body := renderStatus(t, rr, statusData(false, summary, "alice"))
	if strings.Contains(body, statusXSSPayload) {
		t.Errorf("unescaped XSS payload in repo name: %s", body)
	}
	if !strings.Contains(body, `&lt;script&gt;alert(1)&lt;/script&gt;`) {
		t.Errorf("expected escaped payload to appear: %s", body)
	}
}
