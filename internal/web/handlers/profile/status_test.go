// SPDX-License-Identifier: AGPL-3.0-or-later

package profile_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
)

// TestStatusPage_FreeUserUnderEnforceServesTeaser confirms the
// locked-UI gate: a Free user with the enforce flag on gets the
// placeholder/teaser page rather than the live aggregate. The
// teaser banner must be present so the upgrade affordance is
// visible.
func TestStatusPage_FreeUserUnderEnforceServesTeaser(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{
		UserPersonalStatusPage: true,
	})
	alice := env.insertUser(t, "freebie", "Freebie", "")

	body := env.getAs(t, "/freebie/status", alice)
	if !strings.Contains(body, "TEASER=1") {
		t.Errorf("expected teaser marker in body: %s", body)
	}
	if !strings.Contains(body, "STATE=ok") {
		// teaserSummary always returns ok — guards against a
		// regression that swapped in the live aggregator (which
		// would return unknown for a user with no pins).
		t.Errorf("teaser overall state missing: %s", body)
	}
}

// TestStatusPage_FreeUserReportOnlyServesLive: report-only soak runs
// the live aggregate even for Free users so the data path is
// validated before flipping the enforce flag. The page reports
// "unknown" (no pinned repos, no runs).
func TestStatusPage_FreeUserReportOnlyServesLive(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{
		UserPersonalStatusPage: false,
	})
	alice := env.insertUser(t, "soakuser", "Soak", "")

	body := env.getAs(t, "/soakuser/status", alice)
	if strings.Contains(body, "TEASER=1") {
		t.Errorf("expected live (not teaser) under report-only: %s", body)
	}
	if !strings.Contains(body, "STATE=unknown") {
		t.Errorf("expected unknown overall state for empty live aggregate: %s", body)
	}
}

// TestStatusPage_ProUserServesLive asserts the happy path: a Pro
// user always sees the live aggregate regardless of the enforce
// flag, and the teaser banner is suppressed.
func TestStatusPage_ProUserServesLive(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{
		UserPersonalStatusPage: true,
	})
	pro := env.insertUser(t, "paidalice", "Alice", "")
	upgradeUserToProForTest(t, env, pro.ID)

	body := env.getAs(t, "/paidalice/status", pro)
	if strings.Contains(body, "TEASER=1") {
		t.Errorf("Pro user must never see teaser: %s", body)
	}
	// No pinned repos → live aggregate returns unknown. The
	// distinguishing signal vs the teaser is the absence of
	// TEASER=1 — STATE=unknown alone is consistent with both.
}

// TestStatusBadge_FreeUserUnderEnforceServesPaidBadge: badge endpoint
// returns the 402-style "Pro feature" SVG for Free + enforce on. The
// content-type stays image/svg+xml regardless of the gate decision so
// README renderers don't break.
func TestStatusBadge_FreeUserUnderEnforceServesPaidBadge(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{
		UserPersonalStatusPage: true,
	})
	env.insertUser(t, "badgefree", "B", "")

	resp := env.getRawResponse(t, "/badgefree.status.svg")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "image/svg+xml") {
		t.Errorf("Content-Type=%q, want image/svg+xml*", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Pro feature") {
		t.Errorf("expected 'Pro feature' in svg body: %s", body)
	}
}

// TestStatusBadge_UnknownUserStillReturnsSVG: missing user → unknown
// badge, not 404. README images that point at a since-deleted account
// shouldn't visually break the page that embeds them.
func TestStatusBadge_UnknownUserStillReturnsSVG(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{})

	resp := env.getRawResponse(t, "/nosuchuser.status.svg")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (badges always 200)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), ">unknown<") {
		t.Errorf("expected 'unknown' state text in svg: %s", body)
	}
}

// getRawResponse is like getAs but returns the *http.Response without
// asserting status — the badge tests assert their own status codes
// and content types.
func (e *profileEnv) getRawResponse(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := newNonRedirClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	return resp
}
