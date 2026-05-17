// SPDX-License-Identifier: AGPL-3.0-or-later

package repo_test

// PRO-EXT01-15: production HTML render test for the Pause section of
// the repo Danger Zone settings page. Mirrors the PRO-EXT_SR-07
// pattern — render the real template (loaded via web.TemplatesFS())
// so a regression that swaps the disabled attribute or drops the Pro
// badge is caught at the template boundary.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func newSettingsRenderer(t *testing.T) *render.Renderer {
	t.Helper()
	rr, err := render.New(web.TemplatesFS(), render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	return rr
}

func settingsData(repo reposdb.Repo, pauseAllowed bool) map[string]any {
	//nolint:gosec // test fixture, not a credential.
	const fakeCSRF = "test-csrf-token"
	return map[string]any{
		"Title":           "Settings",
		"Viewer":          middleware.CurrentUser{},
		"CSRFToken":       fakeCSRF,
		"Owner":           "alice",
		"Repo":            repo,
		"Transfers":       nil,
		"SettingsActive":  "danger",
		"PauseAllowed":    pauseAllowed,
		"PauseFeatureKey": "repo_time_machine",
	}
}

func renderSettings(t *testing.T, rr *render.Renderer, data map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := rr.Render(&buf, "repo/settings", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// TestSettingsTemplate_PauseEnabledForPro asserts the Pause form
// renders without the locked attributes for a Pro owner. The form
// action and the reason input must both be present + enabled.
func TestSettingsTemplate_PauseEnabledForPro(t *testing.T) {
	t.Parallel()
	rr := newSettingsRenderer(t)
	repo := reposdb.Repo{ID: 1, Name: "demo", DefaultBranch: "trunk", Visibility: reposdb.RepoVisibilityPublic}
	body := renderSettings(t, rr, settingsData(repo, true))

	if !strings.Contains(body, `action="/alice/demo/settings/pause"`) {
		t.Errorf("missing pause form action: %s", truncate(body, 800))
	}
	if !strings.Contains(body, `name="pause_reason"`) {
		t.Errorf("missing pause_reason input")
	}
	if strings.Contains(body, `disabled aria-disabled="true"`) {
		t.Errorf("Pro user should not see disabled controls in pause section")
	}
}

// TestSettingsTemplate_PauseLockedForFree confirms the Free user
// gets the Pro-lock wrapper + disabled inputs + upgrade CTA.
func TestSettingsTemplate_PauseLockedForFree(t *testing.T) {
	t.Parallel()
	rr := newSettingsRenderer(t)
	repo := reposdb.Repo{ID: 1, Name: "demo", DefaultBranch: "trunk", Visibility: reposdb.RepoVisibilityPublic}
	body := renderSettings(t, rr, settingsData(repo, false))

	if !strings.Contains(body, `shithub-pro-lock`) {
		t.Errorf("missing Pro-lock wrapper: %s", truncate(body, 800))
	}
	if !strings.Contains(body, `data-pro-feature="repo_time_machine"`) {
		t.Errorf("missing feature key data-attr")
	}
	if !strings.Contains(body, `disabled aria-disabled="true"`) {
		t.Errorf("Free user should see disabled controls")
	}
	if !strings.Contains(body, `href="/settings/billing"`) {
		t.Errorf("missing upgrade CTA")
	}
}

// TestSettingsTemplate_PausedShowsUnpause: once paused, the section
// flips to an Unpause button + displays the optional pause_reason.
func TestSettingsTemplate_PausedShowsUnpause(t *testing.T) {
	t.Parallel()
	rr := newSettingsRenderer(t)
	repo := reposdb.Repo{
		ID: 1, Name: "demo", DefaultBranch: "trunk",
		Visibility: reposdb.RepoVisibilityPublic,
		IsPaused:   true,
		PauseReason: pgtype.Text{
			String: "winter break — back in March",
			Valid:  true,
		},
	}
	body := renderSettings(t, rr, settingsData(repo, true))

	if !strings.Contains(body, `action="/alice/demo/settings/unpause"`) {
		t.Errorf("missing unpause form action: %s", truncate(body, 800))
	}
	if !strings.Contains(body, `winter break`) {
		t.Errorf("missing rendered pause reason")
	}
}

// TestSettingsTemplate_ArchivedHidesPauseForm: an archived repo
// can't be paused (DB-enforced mutex). The template surfaces a
// "unarchive first" hint instead of the pause form.
func TestSettingsTemplate_ArchivedHidesPauseForm(t *testing.T) {
	t.Parallel()
	rr := newSettingsRenderer(t)
	repo := reposdb.Repo{
		ID: 1, Name: "demo", DefaultBranch: "trunk",
		Visibility: reposdb.RepoVisibilityPublic,
		IsArchived: true,
	}
	body := renderSettings(t, rr, settingsData(repo, true))

	if strings.Contains(body, `name="pause_reason"`) {
		t.Errorf("pause form should not render on archived repo")
	}
	if !strings.Contains(body, `Unarchive first`) {
		t.Errorf("expected unarchive-first hint on archived repo")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
