// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
)

func TestAppliesToOrgOnlyFeatures(t *testing.T) {
	t.Parallel()
	orgOnly := []entitlements.Feature{
		entitlements.FeatureSecretTeams,
		entitlements.FeatureActionsOrgSecrets,
		entitlements.FeatureActionsOrgVariables,
		entitlements.FeaturePrivateCollaboration,
		entitlements.FeatureStorageQuota,
		entitlements.FeatureActionsMinutesQuota,
		entitlements.FeatureScheduledReminders,
		entitlements.FeatureSecretScanning,
		entitlements.FeatureSecretPushProtection,
		entitlements.FeatureSecretCustomPatterns,
		entitlements.FeatureSecretBypassControls,
		entitlements.FeatureCodeScanning,
		entitlements.FeatureSecurityCampaigns,
	}
	for _, f := range orgOnly {
		if !entitlements.FeatureAppliesToKind(f, billing.SubjectKindOrg) {
			t.Errorf("%s should apply to org kind", f)
		}
		if entitlements.FeatureAppliesToKind(f, billing.SubjectKindUser) {
			t.Errorf("%s should NOT apply to user kind", f)
		}
	}
}

func TestAppliesToCrossKindFeatures(t *testing.T) {
	t.Parallel()
	crossKind := []entitlements.Feature{
		entitlements.FeatureAdvancedBranchProtection,
		entitlements.FeatureRequiredReviewers,
		entitlements.FeatureCodeOwnersReview,
	}
	for _, f := range crossKind {
		if !entitlements.FeatureAppliesToKind(f, billing.SubjectKindOrg) {
			t.Errorf("%s should apply to org kind", f)
		}
		if !entitlements.FeatureAppliesToKind(f, billing.SubjectKindUser) {
			t.Errorf("%s should apply to user kind", f)
		}
	}
}

// TestAppliesToUserOnlyFeatures locks the PRO07-introduced user-only
// features. FeatureProfilePinsBeyondFree must not be queryable for
// org kind — orgs share the visible Free cap (PRO01 ratification).
func TestAppliesToUserOnlyFeatures(t *testing.T) {
	t.Parallel()
	userOnly := []entitlements.Feature{
		entitlements.FeatureProfilePinsBeyondFree,
	}
	for _, f := range userOnly {
		if !entitlements.FeatureAppliesToKind(f, billing.SubjectKindUser) {
			t.Errorf("%s should apply to user kind", f)
		}
		if entitlements.FeatureAppliesToKind(f, billing.SubjectKindOrg) {
			t.Errorf("%s should NOT apply to org kind", f)
		}
	}
}

func TestDeprecatedAliasesPointAtCanonical(t *testing.T) {
	t.Parallel()
	// SP-era constants are aliases for the renamed ones. Code that
	// uses FeatureOrgSecretTeams must read the same value as
	// FeatureSecretTeams.
	cases := []struct {
		legacy, canonical entitlements.Feature
	}{
		{entitlements.FeatureOrgSecretTeams, entitlements.FeatureSecretTeams},
		{entitlements.FeatureOrgAdvancedBranchProtection, entitlements.FeatureAdvancedBranchProtection},
		{entitlements.FeatureOrgRequiredReviewers, entitlements.FeatureRequiredReviewers},
		{entitlements.FeatureOrgActionsSecrets, entitlements.FeatureActionsOrgSecrets},
		{entitlements.FeatureOrgActionsVariables, entitlements.FeatureActionsOrgVariables},
		{entitlements.FeatureOrgPrivateCollaboration, entitlements.FeaturePrivateCollaboration},
		{entitlements.FeatureOrgStorageQuota, entitlements.FeatureStorageQuota},
		{entitlements.FeatureOrgActionsMinutesQuota, entitlements.FeatureActionsMinutesQuota},
	}
	for _, tc := range cases {
		if tc.legacy != tc.canonical {
			t.Errorf("alias %q != canonical %q", tc.legacy, tc.canonical)
		}
	}
}

// TestRequirePrincipalFeatureUnknownKind verifies the short-circuit
// when the feature doesn't apply to the principal's kind. Org-only
// features on personal repos must allow without logging — they
// represent "this resource type isn't gateable here."
func TestRequirePrincipalFeatureUnknownKind(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	ok, res := entitlements.RequirePrincipalFeature(
		context.Background(),
		w,
		nil, // pool unused on short-circuit path
		billing.PrincipalForUser(42),
		entitlements.FeatureSecretTeams, // org-only
		entitlements.NewRequireOpts("label", ""),
	)
	if !ok {
		t.Fatalf("inapplicable feature should short-circuit ok=true")
	}
	if !res.UnknownKindForFeature {
		t.Errorf("UnknownKindForFeature should be true")
	}
	if res.WouldDenyButLog {
		t.Errorf("WouldDenyButLog should be false for inapplicable feature")
	}
}

// TestPrincipalUpgradeBannerForUser verifies the kind-aware banner
// copy: user kind says "Pro" and points at /settings/billing; org
// kind says "Team" and points at the org settings page.
func TestPrincipalUpgradeBannerForUser(t *testing.T) {
	t.Parallel()
	decision := entitlements.Decision{Reason: entitlements.ReasonUpgradeRequired}
	user := billing.PrincipalForUser(7)
	banner := decision.PrincipalUpgradeBanner("Required reviewers", user, "")
	if banner.ActionHref != "/settings/billing" {
		t.Errorf("user banner href: got %q, want /settings/billing", banner.ActionHref)
	}
	if banner.Message == "" || !contains(banner.Message, "Pro") {
		t.Errorf("user banner message should mention Pro: %q", banner.Message)
	}
}

func TestPrincipalUpgradeBannerForOrg(t *testing.T) {
	t.Parallel()
	decision := entitlements.Decision{Reason: entitlements.ReasonUpgradeRequired}
	org := billing.PrincipalForOrg(7)
	banner := decision.PrincipalUpgradeBanner("Required reviewers", org, "acme")
	if banner.ActionHref != "/organizations/acme/settings/billing" {
		t.Errorf("org banner href: got %q", banner.ActionHref)
	}
	if !contains(banner.Message, "Team") {
		t.Errorf("org banner message should mention Team: %q", banner.Message)
	}
}

// TestRequirePrincipalFeatureWithLogger smoke-tests that the helper
// runs without panic when given a logger and a nil pool — the
// short-circuit path takes effect for inapplicable features.
func TestRequirePrincipalFeatureWithLogger(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	opts := entitlements.NewRequireOpts("label", "acme")
	opts.Logger = logger
	w := httptest.NewRecorder()
	ok, _ := entitlements.RequirePrincipalFeature(
		context.Background(),
		w,
		nil,
		billing.PrincipalForUser(1),
		entitlements.FeatureActionsOrgSecrets, // org-only, inapplicable to user
		opts,
	)
	if !ok {
		t.Errorf("inapplicable feature: expected ok=true")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
