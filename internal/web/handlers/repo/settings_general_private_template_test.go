// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
)

// PRO-EXT01-06 — the FeaturePrivateRepoTemplates gate on
// settingsGeneralUpdate. Free owners marking a *private* repo as
// is_template should be coerced back to false when the operator has
// flipped the enforce knob; report-only mode (default) lets the write
// land but logs the would-deny.

// TestSettingsGeneral_PrivateTemplateReportOnlyAllowsWrite confirms that
// in the default report-only mode, a Free owner can still flip
// is_template=true on a private repo (the gate just logs).
func TestSettingsGeneral_PrivateTemplateReportOnlyAllowsWrite(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t) // zero-value enforce ⇒ report-only
	mux := f.generalSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/private-repo/settings/general", url.Values{
		"description": {"a private template"},
		"is_template": {"on"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/alice/private-repo/settings/general?notice=saved" {
		t.Fatalf("redirect: got %q, want saved notice", got)
	}
	assertRepoIsTemplate(t, f, f.privateRepo.ID, true)
}

// TestSettingsGeneral_PrivateTemplateEnforcedBlocksFree asserts the
// enforce path coerces is_template back to false and surfaces the
// upgrade-Pro notice when a Free owner posts is_template=on on a
// private repo.
func TestSettingsGeneral_PrivateTemplateEnforcedBlocksFree(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{UserPrivateRepoTemplates: true})
	mux := f.generalSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/private-repo/settings/general", url.Values{
		"description": {"a private template"},
		"is_template": {"on"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/alice/private-repo/settings/general?notice=private-template-upgrade-pro" {
		t.Fatalf("redirect: got %q, want private-template-upgrade-pro notice", got)
	}
	assertRepoIsTemplate(t, f, f.privateRepo.ID, false)
}

// TestSettingsGeneral_PrivateTemplateEnforcedAllowsPro confirms a Pro
// owner is not blocked even with enforce on.
func TestSettingsGeneral_PrivateTemplateEnforcedAllowsPro(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{UserPrivateRepoTemplates: true})
	upgradeRepoFixtureOwnerToProForTemplate(t, f)
	mux := f.generalSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/private-repo/settings/general", url.Values{
		"description": {"a private template"},
		"is_template": {"on"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/alice/private-repo/settings/general?notice=saved" {
		t.Fatalf("redirect: got %q, want saved notice", got)
	}
	assertRepoIsTemplate(t, f, f.privateRepo.ID, true)
}

// TestSettingsGeneral_PublicRepoTemplateUnaffected confirms the gate
// does not block public repos — only private templates are gated.
func TestSettingsGeneral_PublicRepoTemplateUnaffected(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{UserPrivateRepoTemplates: true})
	mux := f.generalSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/public-repo/settings/general", url.Values{
		"description": {"a public template"},
		"is_template": {"on"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/alice/public-repo/settings/general?notice=saved" {
		t.Fatalf("redirect: got %q, want saved notice", got)
	}
	assertRepoIsTemplate(t, f, f.publicRepo.ID, true)
}

// TestPrivateTemplateLocked_FreeOwnerPrivate verifies the locked-UI
// signal flips true for a Free owner of a private repo (drives the
// pro-lock template branch).
func TestPrivateTemplateLocked_FreeOwnerPrivate(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	if !f.handlers.privateTemplateLocked(context.Background(), f.privateRepo) {
		t.Fatalf("privateTemplateLocked should be true for Free + private")
	}
	if f.handlers.privateTemplateLocked(context.Background(), f.publicRepo) {
		t.Fatalf("privateTemplateLocked should be false for public")
	}
}

// TestPrivateTemplateLocked_ProOwnerUnlocks verifies the locked-UI
// signal flips false once the owner upgrades.
func TestPrivateTemplateLocked_ProOwnerUnlocks(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	upgradeRepoFixtureOwnerToProForTemplate(t, f)
	if f.handlers.privateTemplateLocked(context.Background(), f.privateRepo) {
		t.Fatalf("privateTemplateLocked should be false for Pro owner")
	}
}

// upgradeRepoFixtureOwnerToProForTemplate flips the fixture owner to
// Pro for the duration of the test. Distinct from
// upgradeRepoFixtureOwnerToPro (PRO07's branch-protection helper) so
// the parallel-test stripe IDs don't collide.
func upgradeRepoFixtureOwnerToProForTemplate(t *testing.T, f *repoFixture) {
	t.Helper()
	now := time.Now().UTC()
	suffix := strconv.FormatInt(f.owner.ID, 10)
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), f.pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               f.owner.ID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_tpl_pro_" + suffix, Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_tpl_pro_" + suffix,
	})
	if err != nil {
		t.Fatalf("upgrade owner to Pro: %v", err)
	}
}
