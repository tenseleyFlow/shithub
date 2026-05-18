// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestCheckOrgFeature(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(context.Context, billing.Deps, int64, time.Time) error
		now    func(time.Time) time.Time
		want   bool
		reason entitlements.Reason
	}{
		{
			name:   "free org requires upgrade",
			want:   false,
			reason: entitlements.ReasonUpgradeRequired,
		},
		{
			name: "team active allows feature",
			mutate: func(ctx context.Context, deps billing.Deps, orgID int64, now time.Time) error {
				return setSubscription(ctx, deps, orgID, now, billing.PlanTeam, billing.SubscriptionStatusActive, "active")
			},
			want:   true,
			reason: entitlements.ReasonNone,
		},
		{
			name: "team trialing allows feature",
			mutate: func(ctx context.Context, deps billing.Deps, orgID int64, now time.Time) error {
				return setSubscription(ctx, deps, orgID, now, billing.PlanTeam, billing.SubscriptionStatusTrialing, "trialing")
			},
			want:   true,
			reason: entitlements.ReasonNone,
		},
		{
			name: "team incomplete needs billing action",
			mutate: func(ctx context.Context, deps billing.Deps, orgID int64, now time.Time) error {
				return setSubscription(ctx, deps, orgID, now, billing.PlanTeam, billing.SubscriptionStatusIncomplete, "incomplete")
			},
			want:   false,
			reason: entitlements.ReasonBillingActionNeeded,
		},
		{
			name: "team past due within grace still allows feature",
			mutate: func(ctx context.Context, deps billing.Deps, orgID int64, now time.Time) error {
				if err := setSubscription(ctx, deps, orgID, now, billing.PlanTeam, billing.SubscriptionStatusActive, "grace"); err != nil {
					return err
				}
				_, err := billing.MarkPastDue(ctx, deps, orgID, now.Add(24*time.Hour), "evt_past_due")
				return err
			},
			now:    func(now time.Time) time.Time { return now.Add(12 * time.Hour) },
			want:   true,
			reason: entitlements.ReasonNone,
		},
		{
			name: "team past due after grace needs billing action",
			mutate: func(ctx context.Context, deps billing.Deps, orgID int64, now time.Time) error {
				if err := setSubscription(ctx, deps, orgID, now, billing.PlanTeam, billing.SubscriptionStatusActive, "lapsed"); err != nil {
					return err
				}
				_, err := billing.MarkPastDue(ctx, deps, orgID, now.Add(24*time.Hour), "evt_past_due")
				return err
			},
			now:    func(now time.Time) time.Time { return now.Add(48 * time.Hour) },
			want:   false,
			reason: entitlements.ReasonBillingActionNeeded,
		},
		{
			name: "team locked without grace needs billing action",
			mutate: func(ctx context.Context, deps billing.Deps, orgID int64, now time.Time) error {
				return setSubscription(ctx, deps, orgID, now, billing.PlanTeam, billing.SubscriptionStatusPastDue, "locked")
			},
			want:   false,
			reason: entitlements.ReasonBillingActionNeeded,
		},
		{
			name: "enterprise stub does not unlock team features",
			mutate: func(ctx context.Context, deps billing.Deps, orgID int64, now time.Time) error {
				return setSubscription(ctx, deps, orgID, now, billing.PlanEnterprise, billing.SubscriptionStatusActive, "enterprise")
			},
			want:   false,
			reason: entitlements.ReasonEnterpriseContactSales,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			pool, orgID := setupEntitlementOrg(t)
			bdeps := billing.Deps{Pool: pool}
			now := time.Now().UTC().Truncate(time.Second)
			if tt.mutate != nil {
				if err := tt.mutate(ctx, bdeps, orgID, now); err != nil {
					t.Fatalf("mutate billing state: %v", err)
				}
			}
			checkNow := now
			if tt.now != nil {
				checkNow = tt.now(now)
			}
			decision, err := entitlements.CheckOrgFeature(ctx, entitlements.Deps{
				Pool: pool,
				Now:  func() time.Time { return checkNow },
			}, orgID, entitlements.FeatureOrgActionsSecrets)
			if err != nil {
				t.Fatalf("CheckOrgFeature: %v", err)
			}
			if decision.Allowed != tt.want || decision.Reason != tt.reason {
				t.Fatalf("decision = %+v, want allowed=%v reason=%s", decision, tt.want, tt.reason)
			}
		})
	}
}

func TestForOrgCanUseAndLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, orgID := setupEntitlementOrg(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := setSubscription(ctx, billing.Deps{Pool: pool}, orgID, now, billing.PlanTeam, billing.SubscriptionStatusActive, "limits"); err != nil {
		t.Fatalf("set subscription: %v", err)
	}

	set, err := entitlements.ForOrg(ctx, entitlements.Deps{
		Pool: pool,
		Now:  func() time.Time { return now },
	}, orgID)
	if err != nil {
		t.Fatalf("ForOrg: %v", err)
	}
	for _, feature := range []entitlements.Feature{
		entitlements.FeatureOrgSecretTeams,
		entitlements.FeatureOrgAdvancedBranchProtection,
		entitlements.FeatureOrgRequiredReviewers,
		entitlements.FeatureOrgActionsSecrets,
		entitlements.FeatureOrgActionsVariables,
		entitlements.FeatureOrgPrivateCollaboration,
		entitlements.FeatureOrgStorageQuota,
		entitlements.FeatureOrgActionsMinutesQuota,
		entitlements.FeatureScheduledReminders,
		entitlements.FeatureRepoProjects,
		entitlements.FeatureRepoWikis,
		entitlements.FeatureRepoInsights,
		entitlements.FeatureMultipleAssignees,
	} {
		if decision := set.CanUse(feature); !decision.Allowed {
			t.Fatalf("feature %s decision=%+v, want allowed", feature, decision)
		}
	}
	collab, err := set.Limit(entitlements.LimitOrgPrivateCollaboration)
	if err != nil {
		t.Fatalf("Limit private collaboration: %v", err)
	}
	if !collab.Allowed || !collab.Defined || !collab.Unlimited || collab.Unit != "collaborators" {
		t.Fatalf("private collaboration limit = %+v", collab)
	}
	storage, err := set.Limit(entitlements.LimitOrgStorageQuota)
	if err != nil {
		t.Fatalf("Limit storage: %v", err)
	}
	if !storage.Allowed || !storage.Defined || storage.Value != entitlements.TeamOrgStorageQuotaBytes || storage.Unit != "bytes" {
		t.Fatalf("storage limit = %+v, want Team concrete quota", storage)
	}
	minutes, err := set.Limit(entitlements.LimitOrgActionsMinutesQuota)
	if err != nil {
		t.Fatalf("Limit actions minutes: %v", err)
	}
	if !minutes.Allowed || !minutes.Defined || minutes.Value != entitlements.TeamOrgActionsMinutesQuota || minutes.Unit != "minutes" {
		t.Fatalf("actions minutes limit = %+v, want Team concrete quota", minutes)
	}
}

func TestUsageLimitsExposeFreeQuotas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, orgID := setupEntitlementOrg(t)
	set, err := entitlements.ForOrg(ctx, entitlements.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("ForOrg: %v", err)
	}
	storage, err := set.Limit(entitlements.LimitOrgStorageQuota)
	if err != nil {
		t.Fatalf("Limit storage: %v", err)
	}
	if !storage.Allowed || !storage.Defined || storage.Value != entitlements.FreeOrgStorageQuotaBytes || storage.RequiredPlan != billing.PlanTeam {
		t.Fatalf("free storage limit = %+v", storage)
	}
	minutes, err := set.Limit(entitlements.LimitOrgActionsMinutesQuota)
	if err != nil {
		t.Fatalf("Limit actions minutes: %v", err)
	}
	if !minutes.Allowed || !minutes.Defined || minutes.Value != entitlements.FreeOrgActionsMinutesQuota || minutes.RequiredPlan != billing.PlanTeam {
		t.Fatalf("free actions minutes limit = %+v", minutes)
	}
}

func TestUsageLimitsApplyQuotaOverrides(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, orgID := setupEntitlementOrg(t)
	deps := billing.Deps{Pool: pool}
	if _, err := billing.UpsertOrgQuotaOverride(ctx, deps, billing.QuotaOverrideInput{
		OrgID:           orgID,
		Kind:            billing.QuotaKindStorageBytes,
		LimitValue:      10 * 1024 * 1024 * 1024,
		CreatedByUserID: 1,
	}); err != nil {
		t.Fatalf("UpsertOrgQuotaOverride storage: %v", err)
	}
	if _, err := billing.UpsertOrgQuotaOverride(ctx, deps, billing.QuotaOverrideInput{
		OrgID:           orgID,
		Kind:            billing.QuotaKindActionsMinutes,
		Unlimited:       true,
		CreatedByUserID: 1,
	}); err != nil {
		t.Fatalf("UpsertOrgQuotaOverride minutes: %v", err)
	}

	set, err := entitlements.ForOrg(ctx, entitlements.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("ForOrg: %v", err)
	}
	storage, err := set.Limit(entitlements.LimitOrgStorageQuota)
	if err != nil {
		t.Fatalf("Limit storage: %v", err)
	}
	if !storage.Allowed || !storage.Defined || !storage.Overridden || storage.Value != 10*1024*1024*1024 || storage.Unlimited {
		t.Fatalf("storage override limit = %+v", storage)
	}
	minutes, err := set.Limit(entitlements.LimitOrgActionsMinutesQuota)
	if err != nil {
		t.Fatalf("Limit actions minutes: %v", err)
	}
	if !minutes.Allowed || !minutes.Defined || !minutes.Overridden || !minutes.Unlimited || minutes.Value != 0 {
		t.Fatalf("actions minutes override limit = %+v", minutes)
	}
}

func TestCheckOrgStorageQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, orgID := setupEntitlementOrg(t)
	check, err := entitlements.CheckOrgStorageQuota(ctx, entitlements.Deps{Pool: pool}, orgID, 1024, 512)
	if err != nil {
		t.Fatalf("CheckOrgStorageQuota: %v", err)
	}
	if !check.Allowed || check.WouldUseBytes != 1536 || check.LimitBytes != entitlements.FreeOrgStorageQuotaBytes {
		t.Fatalf("free storage check = %+v", check)
	}

	if _, err := billing.UpsertOrgQuotaOverride(ctx, billing.Deps{Pool: pool}, billing.QuotaOverrideInput{
		OrgID:           orgID,
		Kind:            billing.QuotaKindStorageBytes,
		LimitValue:      1000,
		CreatedByUserID: 1,
	}); err != nil {
		t.Fatalf("UpsertOrgQuotaOverride storage: %v", err)
	}
	check, err = entitlements.CheckOrgStorageQuota(ctx, entitlements.Deps{Pool: pool}, orgID, 900, 101)
	if err != nil {
		t.Fatalf("CheckOrgStorageQuota override: %v", err)
	}
	if check.Allowed || check.WouldUseBytes != 1001 || check.LimitBytes != 1000 || !check.Overridden {
		t.Fatalf("over-limit storage check = %+v", check)
	}
	if !errors.Is(check.Err(), entitlements.ErrOrgStorageQuotaExceeded) {
		t.Fatalf("check.Err() = %v, want ErrOrgStorageQuotaExceeded", check.Err())
	}

	if _, err := billing.UpsertOrgQuotaOverride(ctx, billing.Deps{Pool: pool}, billing.QuotaOverrideInput{
		OrgID:           orgID,
		Kind:            billing.QuotaKindStorageBytes,
		Unlimited:       true,
		CreatedByUserID: 1,
	}); err != nil {
		t.Fatalf("UpsertOrgQuotaOverride unlimited storage: %v", err)
	}
	check, err = entitlements.CheckOrgStorageQuota(ctx, entitlements.Deps{Pool: pool}, orgID, 1<<40, 1<<40)
	if err != nil {
		t.Fatalf("CheckOrgStorageQuota unlimited: %v", err)
	}
	if !check.Allowed || !check.Unlimited || !check.Overridden {
		t.Fatalf("unlimited storage check = %+v", check)
	}
}

func TestUnknownFeatureAndLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, orgID := setupEntitlementOrg(t)
	_, err := entitlements.CheckOrgFeature(ctx, entitlements.Deps{Pool: pool}, orgID, entitlements.Feature("org.mystery"))
	if !errors.Is(err, entitlements.ErrUnknownFeature) {
		t.Fatalf("CheckOrgFeature unknown err=%v, want ErrUnknownFeature", err)
	}
	set, err := entitlements.ForOrg(ctx, entitlements.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("ForOrg: %v", err)
	}
	_, err = set.Limit(entitlements.Limit("org.mystery_limit"))
	if !errors.Is(err, entitlements.ErrUnknownLimit) {
		t.Fatalf("Limit unknown err=%v, want ErrUnknownLimit", err)
	}
}

func TestDecisionUpgradeBanner(t *testing.T) {
	t.Parallel()
	decision := entitlements.Decision{
		Feature:      entitlements.FeatureOrgSecretTeams,
		RequiredPlan: billing.PlanTeam,
		Reason:       entitlements.ReasonUpgradeRequired,
	}
	banner := decision.UpgradeBanner("Secret teams", "acme inc")
	if banner.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status=%d, want 402", banner.StatusCode)
	}
	if banner.ActionHref != "/organizations/acme%20inc/settings/billing" {
		t.Fatalf("href=%q", banner.ActionHref)
	}
	if !strings.Contains(banner.Message, "require Team billing") {
		t.Fatalf("message=%q", banner.Message)
	}
}

func setSubscription(ctx context.Context, deps billing.Deps, orgID int64, now time.Time, plan billing.Plan, status billing.SubscriptionStatus, suffix string) error {
	_, err := billing.ApplySubscriptionSnapshot(ctx, deps, billing.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     plan,
		Status:                   status,
		StripeSubscriptionID:     "sub_" + suffix,
		StripeSubscriptionItemID: "si_" + suffix,
		CurrentPeriodStart:       now,
		CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_" + suffix,
	})
	return err
}

func setupEntitlementOrg(t *testing.T) (*pgxpool.Pool, int64) {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "owner", DisplayName: "Owner", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	org, err := orgs.Create(ctx, orgs.Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme Inc", CreatedByUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	return pool, org.ID
}
