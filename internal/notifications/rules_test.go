// SPDX-License-Identifier: AGPL-3.0-or-later

package notifications_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	"github.com/tenseleyFlow/shithub/internal/notifications"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestEvaluator_ProUserMatchingRuleFires: a Pro user with one
// matching rule gets a Decision back. Validates the end-to-end DB
// path (entitlement check + rule load + matcher).
func TestEvaluator_ProUserMatchingRuleFires(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "pro-rules")
	upgradeToPro(t, pool, user.ID)

	rule := insertRule(t, pool, user.ID, notifdb.InsertUserNotificationRuleParams{
		UserID:      user.ID,
		Name:        "mute the bot",
		Enabled:     true,
		Position:    0,
		MatchReason: pgtype.Text{String: "mention", Valid: true},
		Action:      notifdb.UserNotificationRuleActionTab,
		ActionTab:   pgtype.Text{String: "bots", Valid: true},
	})
	n := notifdb.Notification{
		RecipientUserID: user.ID,
		Reason:          "mention",
		Kind:            "issue_comment_created",
	}
	e := &notifications.Evaluator{Pool: pool}
	got, err := e.Evaluate(ctx, n)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got.RuleID != rule.ID {
		t.Errorf("RuleID = %d, want %d", got.RuleID, rule.ID)
	}
	if got.Tab != "bots" {
		t.Errorf("Tab = %q, want %q", got.Tab, "bots")
	}
}

// TestEvaluator_FreeUserReportOnlyStillFires confirms the soak
// path: Free users' rules continue to apply (with a deny log) until
// the operator flips EnforceFree.
func TestEvaluator_FreeUserReportOnlyStillFires(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "free-soak")
	// No upgrade — user stays Free.

	rule := insertRule(t, pool, user.ID, notifdb.InsertUserNotificationRuleParams{
		UserID:      user.ID,
		Name:        "test",
		Enabled:     true,
		Position:    0,
		MatchReason: pgtype.Text{String: "mention", Valid: true},
		Action:      notifdb.UserNotificationRuleActionMarkRead,
	})
	n := notifdb.Notification{RecipientUserID: user.ID, Reason: "mention"}

	e := &notifications.Evaluator{
		Pool:        pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		EnforceFree: false,
	}
	got, err := e.Evaluate(ctx, n)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got.RuleID != rule.ID {
		t.Errorf("Free + report-only: RuleID = %d, want %d", got.RuleID, rule.ID)
	}
}

// TestEvaluator_FreeUserEnforceDoesNotFire confirms the post-soak
// path: EnforceFree=true blocks Free users' rules even when they
// exist (e.g. from a Pro→Free downgrade).
func TestEvaluator_FreeUserEnforceDoesNotFire(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "free-enforce")

	insertRule(t, pool, user.ID, notifdb.InsertUserNotificationRuleParams{
		UserID: user.ID, Name: "test", Enabled: true, Position: 0,
		Action: notifdb.UserNotificationRuleActionMarkRead,
	})
	n := notifdb.Notification{RecipientUserID: user.ID, Reason: "mention"}

	e := &notifications.Evaluator{
		Pool:        pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		EnforceFree: true,
	}
	got, err := e.Evaluate(ctx, n)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got.RuleID != 0 {
		t.Errorf("Free + enforce: RuleID = %d, want 0 (blocked)", got.RuleID)
	}
}

// TestEvaluator_DisabledRuleSkipped: rules with enabled=false don't
// fire. Validates the partial-index-backed
// ListEnabledUserNotificationRules query.
func TestEvaluator_DisabledRuleSkipped(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "disabled-rule")
	upgradeToPro(t, pool, user.ID)

	insertRule(t, pool, user.ID, notifdb.InsertUserNotificationRuleParams{
		UserID: user.ID, Name: "disabled", Enabled: false, Position: 0,
		MatchReason: pgtype.Text{String: "mention", Valid: true},
		Action:      notifdb.UserNotificationRuleActionMarkRead,
	})
	n := notifdb.Notification{RecipientUserID: user.ID, Reason: "mention"}

	e := &notifications.Evaluator{Pool: pool}
	got, err := e.Evaluate(ctx, n)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got.RuleID != 0 {
		t.Errorf("disabled rule fired: RuleID = %d, want 0", got.RuleID)
	}
}

// TestEvaluator_PositionOrderFirstMatchWins: two rules both match;
// the lower-position rule should win.
func TestEvaluator_PositionOrderFirstMatchWins(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "order-test")
	upgradeToPro(t, pool, user.ID)

	first := insertRule(t, pool, user.ID, notifdb.InsertUserNotificationRuleParams{
		UserID: user.ID, Name: "first", Enabled: true, Position: 0,
		MatchReason: pgtype.Text{String: "mention", Valid: true},
		Action:      notifdb.UserNotificationRuleActionTab,
		ActionTab:   pgtype.Text{String: "primary", Valid: true},
	})
	insertRule(t, pool, user.ID, notifdb.InsertUserNotificationRuleParams{
		UserID: user.ID, Name: "second", Enabled: true, Position: 1,
		MatchReason: pgtype.Text{String: "mention", Valid: true},
		Action:      notifdb.UserNotificationRuleActionDrop,
	})
	n := notifdb.Notification{RecipientUserID: user.ID, Reason: "mention"}

	e := &notifications.Evaluator{Pool: pool}
	got, err := e.Evaluate(ctx, n)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got.RuleID != first.ID {
		t.Errorf("position-0 rule should win; got RuleID = %d, want %d", got.RuleID, first.ID)
	}
}

// --- helpers --------------------------------------------------------

func mkUser(t *testing.T, pool *pgxpool.Pool, username string) usersdb.User {
	t.Helper()
	u, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: username, DisplayName: username, PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func upgradeToPro(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	now := time.Now().UTC()
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_test", Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_test",
	})
	if err != nil {
		t.Fatalf("upgrade to Pro: %v", err)
	}
}

func insertRule(t *testing.T, pool *pgxpool.Pool, _ int64, p notifdb.InsertUserNotificationRuleParams) notifdb.UserNotificationRule {
	t.Helper()
	r, err := notifdb.New().InsertUserNotificationRule(context.Background(), pool, p)
	if err != nil {
		t.Fatalf("InsertUserNotificationRule: %v", err)
	}
	return r
}
