// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements_test

import (
	"context"
	"io"
	"log/slog"
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
				_, err := billing.ApplySubscriptionSnapshot(ctx, deps, billing.SubscriptionSnapshot{
					OrgID:                    orgID,
					Plan:                     billing.PlanTeam,
					Status:                   billing.SubscriptionStatusActive,
					StripeSubscriptionID:     "sub_active",
					StripeSubscriptionItemID: "si_active",
					CurrentPeriodStart:       now,
					CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
					LastWebhookEventID:       "evt_active",
				})
				return err
			},
			want:   true,
			reason: entitlements.ReasonNone,
		},
		{
			name: "team past due within grace still allows feature",
			mutate: func(ctx context.Context, deps billing.Deps, orgID int64, now time.Time) error {
				if _, err := billing.ApplySubscriptionSnapshot(ctx, deps, billing.SubscriptionSnapshot{
					OrgID:                    orgID,
					Plan:                     billing.PlanTeam,
					Status:                   billing.SubscriptionStatusActive,
					StripeSubscriptionID:     "sub_grace",
					StripeSubscriptionItemID: "si_grace",
					CurrentPeriodStart:       now,
					CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
					LastWebhookEventID:       "evt_active",
				}); err != nil {
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
				if _, err := billing.ApplySubscriptionSnapshot(ctx, deps, billing.SubscriptionSnapshot{
					OrgID:                    orgID,
					Plan:                     billing.PlanTeam,
					Status:                   billing.SubscriptionStatusActive,
					StripeSubscriptionID:     "sub_lapsed",
					StripeSubscriptionItemID: "si_lapsed",
					CurrentPeriodStart:       now,
					CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
					LastWebhookEventID:       "evt_active",
				}); err != nil {
					return err
				}
				_, err := billing.MarkPastDue(ctx, deps, orgID, now.Add(24*time.Hour), "evt_past_due")
				return err
			},
			now:    func(now time.Time) time.Time { return now.Add(48 * time.Hour) },
			want:   false,
			reason: entitlements.ReasonBillingActionNeeded,
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
