// SPDX-License-Identifier: AGPL-3.0-or-later

package notifications_test

// PRO-EXT01-16c: cross-feature integration tests. The PR-16a tests
// cover the rule engine alone; the PR-16b tests cover the digest
// sweep alone. These tests assert the two features compose correctly
// — a notification that a rule snoozes or drops must NOT appear in
// the digest. Without these, a regression that makes the digest
// ignore the snooze state would land silently.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	"github.com/tenseleyFlow/shithub/internal/notifications"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// TestIntegration_SnoozedNotificationSkippedByDigest is the end-to-
// end promise of focus mode: a rule snoozes a notification → the
// next digest email doesn't include it.
func TestIntegration_SnoozedNotificationSkippedByDigest(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "integ-snooze")
	upgradeToPro(t, pool, user.ID)
	verifyEmail(t, pool, user.ID, "integ-snooze@example.test")

	// Insert two notifications. The first will be snoozed by a rule
	// (won't appear in digest); the second is normal.
	snoozedID := insertUnreadNotificationReturning(t, pool, user.ID, "issue_comment_created", "mention")
	insertUnreadNotificationReturning(t, pool, user.ID, "issue_comment_created", "subscribed")

	// Mark snoozed row as snoozed 1h into the future.
	snoozeUntil := time.Now().Add(1 * time.Hour).UTC()
	if _, err := pool.Exec(ctx,
		`UPDATE notifications SET snoozed_until = $1 WHERE id = $2`,
		pgtype.Timestamptz{Time: snoozeUntil, Valid: true}, snoozedID); err != nil {
		t.Fatalf("apply snooze: %v", err)
	}

	// Past-due digest schedule.
	past := time.Now().Add(-time.Hour).UTC()
	upsertDigest(t, pool, user.ID, true, notifdb.UserNotificationDigestFrequencyDaily, 9, past)

	sender := &captureSender{}
	if _, err := notifications.SweepOnce(ctx, notifications.DigestSweepDeps{
		Pool:        pool,
		EmailSender: sender,
		EmailFrom:   "noreply@shithub.test",
		SiteName:    "shithub",
		BaseURL:     "https://shithub.test",
	}); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if len(sender.Sent()) != 1 {
		t.Fatalf("emails sent = %d, want 1", len(sender.Sent()))
	}
	body := sender.Sent()[0].Text
	// Only the non-snoozed notification (reason=subscribed) should
	// appear in the body. The snoozed one (reason=mention) must not.
	if !contains(body, "(subscribed)") {
		t.Errorf("digest missing the non-snoozed notification:\n%s", body)
	}
	if contains(body, "(mention)") {
		t.Errorf("digest included snoozed notification:\n%s", body)
	}
	// Header should say "1 unread", not "2".
	subj := sender.Sent()[0].Subject
	if !contains(subj, "1 unread notification") {
		t.Errorf("subject = %q, want it to say 1 unread (snoozed excluded)", subj)
	}
}

// TestIntegration_DroppedNotificationSkippedByDigest: same shape
// but with the drop action. A dropped row carries tab_label='dropped'
// and the digest body query excludes it.
func TestIntegration_DroppedNotificationSkippedByDigest(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "integ-drop")
	upgradeToPro(t, pool, user.ID)
	verifyEmail(t, pool, user.ID, "integ-drop@example.test")

	droppedID := insertUnreadNotificationReturning(t, pool, user.ID, "issue_comment_created", "mention")
	insertUnreadNotificationReturning(t, pool, user.ID, "issue_comment_created", "review_requested")

	if _, err := pool.Exec(ctx,
		`UPDATE notifications SET tab_label = 'dropped' WHERE id = $1`, droppedID); err != nil {
		t.Fatalf("apply drop: %v", err)
	}

	past := time.Now().Add(-time.Hour).UTC()
	upsertDigest(t, pool, user.ID, true, notifdb.UserNotificationDigestFrequencyDaily, 9, past)

	sender := &captureSender{}
	if _, err := notifications.SweepOnce(ctx, notifications.DigestSweepDeps{
		Pool:        pool,
		EmailSender: sender,
		EmailFrom:   "noreply@shithub.test",
		SiteName:    "shithub",
		BaseURL:     "https://shithub.test",
	}); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if len(sender.Sent()) != 1 {
		t.Fatalf("emails sent = %d, want 1", len(sender.Sent()))
	}
	if contains(sender.Sent()[0].Text, "(mention)") {
		t.Errorf("digest included dropped notification:\n%s", sender.Sent()[0].Text)
	}
}

// TestIntegration_AwakeSnoozeAppearsInDigest: a notification whose
// snooze has already expired should reappear in the digest. Catches
// a regression where the "snoozed_until > now()" comparison is
// inverted.
func TestIntegration_AwakeSnoozeAppearsInDigest(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "integ-awake")
	upgradeToPro(t, pool, user.ID)
	verifyEmail(t, pool, user.ID, "integ-awake@example.test")

	id := insertUnreadNotificationReturning(t, pool, user.ID, "issue_comment_created", "mention")
	// Snooze that already expired.
	if _, err := pool.Exec(ctx,
		`UPDATE notifications SET snoozed_until = $1 WHERE id = $2`,
		pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour).UTC(), Valid: true}, id); err != nil {
		t.Fatalf("apply expired snooze: %v", err)
	}

	past := time.Now().Add(-time.Hour).UTC()
	upsertDigest(t, pool, user.ID, true, notifdb.UserNotificationDigestFrequencyDaily, 9, past)

	sender := &captureSender{}
	if _, err := notifications.SweepOnce(ctx, notifications.DigestSweepDeps{
		Pool:        pool,
		EmailSender: sender,
		EmailFrom:   "noreply@shithub.test",
		SiteName:    "shithub",
		BaseURL:     "https://shithub.test",
	}); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if len(sender.Sent()) != 1 {
		t.Fatalf("emails sent = %d, want 1 (snooze expired, should fire)", len(sender.Sent()))
	}
}

// --- helpers ---------------------------------------------------------

func insertUnreadNotificationReturning(t *testing.T, pool *pgxpool.Pool, userID int64, kind, reason string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO notifications (recipient_user_id, kind, reason, source_event_id, unread)
		 VALUES ($1, $2, $3, 1, true) RETURNING id`,
		userID, kind, reason).Scan(&id); err != nil {
		t.Fatalf("insert notification: %v", err)
	}
	return id
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
