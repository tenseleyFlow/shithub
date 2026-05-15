// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
)

// PRO-EXT01-07b — exercises the scheduled-issues gate decisions
// (parseScheduleAt + scheduleIssueAccepted) at orchestrator level.
// The full HTTP path is exercised by the worker tests in
// internal/worker/jobs and by manual smoke.

func TestParseScheduleAt_RejectsPast(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	if _, err := parseScheduleAt("2024-01-01T00:00", now); err == nil {
		t.Fatalf("past datetime should be rejected")
	}
}

func TestParseScheduleAt_RejectsTooFarFuture(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	tooFar := now.Add(400 * 24 * time.Hour).Format("2006-01-02T15:04")
	if _, err := parseScheduleAt(tooFar, now); err == nil {
		t.Fatalf("year+1 datetime should be rejected")
	}
}

func TestParseScheduleAt_AcceptsSoonFuture(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	soon := now.Add(30 * time.Minute).Format("2006-01-02T15:04")
	parsed, err := parseScheduleAt(soon, now)
	if err != nil {
		t.Fatalf("near-future should parse: %v", err)
	}
	if parsed.Before(now) {
		t.Errorf("parsed=%v before now=%v", parsed, now)
	}
}

func TestParseScheduleAt_RejectsGarbage(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	if _, err := parseScheduleAt("not a date", now); err == nil {
		t.Fatalf("garbage input should be rejected")
	}
}

// TestScheduleIssueAccepted_ReportOnlyLetsFreeThrough pins the
// default report-only path: a Free user's schedule attempt is honoured
// (the would-deny is logged but the schedule lands).
func TestScheduleIssueAccepted_ReportOnlyLetsFreeThrough(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t) // zero-value enforce
	if !f.handlers.scheduleIssueAccepted(context.Background(), f.owner.ID) {
		t.Fatalf("report-only Free user should be accepted (would-deny logs but proceeds)")
	}
}

// TestScheduleIssueAccepted_EnforceBlocksFree pins the enforce path:
// a Free user with UserScheduledIssues=true is denied.
func TestScheduleIssueAccepted_EnforceBlocksFree(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, configEnforceScheduledIssues())
	if f.handlers.scheduleIssueAccepted(context.Background(), f.owner.ID) {
		t.Fatalf("enforce-mode Free user should be denied")
	}
}

// TestScheduleIssueAccepted_ProAlwaysAllowed verifies a Pro owner
// passes both modes.
func TestScheduleIssueAccepted_ProAlwaysAllowed(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, configEnforceScheduledIssues())
	upgradeRepoFixtureOwnerToProForSchedule(t, f)
	if !f.handlers.scheduleIssueAccepted(context.Background(), f.owner.ID) {
		t.Fatalf("Pro owner should be accepted in enforce mode")
	}
}

// configEnforceScheduledIssues constructs a fixture EnforceConfig with
// UserScheduledIssues=true. Kept in a helper so the test list reads
// cleanly.
func configEnforceScheduledIssues() config.EnforceConfig {
	return config.EnforceConfig{UserScheduledIssues: true}
}

func upgradeRepoFixtureOwnerToProForSchedule(t *testing.T, f *repoFixture) {
	t.Helper()
	now := time.Now().UTC()
	suffix := strconv.FormatInt(f.owner.ID, 10)
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), f.pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               f.owner.ID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_sched_pro_" + suffix, Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_sched_pro_" + suffix,
	})
	if err != nil {
		t.Fatalf("upgrade owner to Pro: %v", err)
	}
}
