// SPDX-License-Identifier: AGPL-3.0-or-later

package notifications_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	"github.com/tenseleyFlow/shithub/internal/notifications"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// verifyEmail inserts + links a verified primary email for the user.
// The digest sweep skips users with no verified primary.
func verifyEmail(t *testing.T, pool *pgxpool.Pool, userID int64, addr string) {
	t.Helper()
	q := usersdb.New()
	em, err := q.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: userID, Email: addr, IsPrimary: true, Verified: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	if err := q.LinkUserPrimaryEmail(context.Background(), pool, usersdb.LinkUserPrimaryEmailParams{
		ID: userID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	}); err != nil {
		t.Fatalf("LinkUserPrimaryEmail: %v", err)
	}
}

// captureSender records every Send call. Stand-in for a real backend
// in tests; the digest sweep should call Send exactly once per
// eligible user with unread notifications.
type captureSender struct {
	mu       sync.Mutex
	messages []email.Message
}

func (s *captureSender) Send(_ context.Context, msg email.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
	return nil
}

func (s *captureSender) Sent() []email.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]email.Message, len(s.messages))
	copy(out, s.messages)
	return out
}

// TestSweepOnce_ProUserWithUnreadGetsDigest is the happy path:
// Pro user has digest enabled + an unread notification → one email
// is sent + next_send_at advances.
func TestSweepOnce_ProUserWithUnreadGetsDigest(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "pro-digest")
	upgradeToPro(t, pool, user.ID)
	verifyEmail(t, pool, user.ID, "pro-digest@example.com")

	// Past-due schedule.
	past := time.Now().Add(-time.Hour).UTC()
	upsertDigest(t, pool, user.ID, true, notifdb.UserNotificationDigestFrequencyDaily, 9, past)
	insertUnreadNotification(t, pool, user.ID, "issue_comment_created", "mention")

	sender := &captureSender{}
	deps := notifications.DigestSweepDeps{
		Pool:        pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmailSender: sender,
		EmailFrom:   "noreply@shithub.test",
		SiteName:    "shithub",
		BaseURL:     "https://shithub.test",
	}
	processed, err := notifications.SweepOnce(ctx, deps)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1", processed)
	}
	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("emails sent = %d, want 1", len(sent))
	}
	if sent[0].To != "pro-digest@example.com" {
		t.Errorf("To = %q, want pro-digest@example.com", sent[0].To)
	}
	if !strings.Contains(sent[0].Subject, "1 unread notification") {
		t.Errorf("Subject = %q, want it to mention 1 unread", sent[0].Subject)
	}

	// Advance check — next_send_at must be in the future.
	row, _ := notifdb.New().GetUserNotificationDigest(ctx, pool, user.ID)
	if !row.NextSendAt.Valid || !row.NextSendAt.Time.After(time.Now()) {
		t.Errorf("next_send_at = %v, want future", row.NextSendAt)
	}
}

// TestSweepOnce_FreeEnforceSkipsButAdvances: under enforce, a Free
// user's row is not delivered but next_send_at still advances so the
// sweep doesn't spin on it.
func TestSweepOnce_FreeEnforceSkipsButAdvances(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "free-enforce-digest")
	verifyEmail(t, pool, user.ID, "free@example.test")

	past := time.Now().Add(-time.Hour).UTC()
	upsertDigest(t, pool, user.ID, true, notifdb.UserNotificationDigestFrequencyDaily, 9, past)
	insertUnreadNotification(t, pool, user.ID, "issue_comment_created", "mention")

	sender := &captureSender{}
	deps := notifications.DigestSweepDeps{
		Pool:           pool,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmailSender:    sender,
		EnforceDigests: true,
	}
	processed, err := notifications.SweepOnce(ctx, deps)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1 (row was claimed + advanced)", processed)
	}
	if len(sender.Sent()) != 0 {
		t.Errorf("emails sent = %d, want 0 (enforce blocked)", len(sender.Sent()))
	}
	row, _ := notifdb.New().GetUserNotificationDigest(ctx, pool, user.ID)
	if !row.NextSendAt.Valid || !row.NextSendAt.Time.After(time.Now()) {
		t.Errorf("next_send_at = %v, want future (must advance even on skip)", row.NextSendAt)
	}
}

// TestSweepOnce_EmptyInboxSkipsEmail: a user with digest enabled but
// no unread notifications doesn't get an empty digest email. The row
// is still advanced.
func TestSweepOnce_EmptyInboxSkipsEmail(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "empty-inbox")
	upgradeToPro(t, pool, user.ID)
	verifyEmail(t, pool, user.ID, "empty@example.test")

	past := time.Now().Add(-time.Hour).UTC()
	upsertDigest(t, pool, user.ID, true, notifdb.UserNotificationDigestFrequencyDaily, 9, past)
	// No notifications inserted.

	sender := &captureSender{}
	deps := notifications.DigestSweepDeps{
		Pool: pool, EmailSender: sender,
		EmailFrom: "noreply@shithub.test", SiteName: "shithub",
	}
	_, err := notifications.SweepOnce(ctx, deps)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if len(sender.Sent()) != 0 {
		t.Errorf("emails sent = %d, want 0 (empty inbox)", len(sender.Sent()))
	}
}

// TestSweepOnce_DisabledRowSkipped: a row with enabled=false isn't
// claimed at all (the partial index excludes it).
func TestSweepOnce_DisabledRowSkipped(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := mkUser(t, pool, "disabled-digest")
	upgradeToPro(t, pool, user.ID)

	past := time.Now().Add(-time.Hour).UTC()
	upsertDigest(t, pool, user.ID, false /* enabled */, notifdb.UserNotificationDigestFrequencyDaily, 9, past)

	sender := &captureSender{}
	deps := notifications.DigestSweepDeps{Pool: pool, EmailSender: sender}
	processed, err := notifications.SweepOnce(ctx, deps)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if processed != 0 {
		t.Errorf("processed = %d, want 0 (disabled row must not be claimed)", processed)
	}
}

// --- helpers ----------------------------------------------------------

func upsertDigest(
	t *testing.T, pool *pgxpool.Pool, userID int64, enabled bool,
	freq notifdb.UserNotificationDigestFrequency, hour int,
	nextSend time.Time,
) {
	t.Helper()
	if _, err := notifdb.New().UpsertUserNotificationDigest(context.Background(), pool, notifdb.UpsertUserNotificationDigestParams{
		UserID:     userID,
		Enabled:    enabled,
		Frequency:  freq,
		HourUtc:    int16(hour),
		NextSendAt: pgtype.Timestamptz{Time: nextSend, Valid: true},
	}); err != nil {
		t.Fatalf("upsertDigest: %v", err)
	}
}

func insertUnreadNotification(t *testing.T, pool *pgxpool.Pool, userID int64, kind, reason string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO notifications (recipient_user_id, kind, reason, source_event_id, unread)
		 VALUES ($1, $2, $3, 1, true)`,
		userID, kind, reason); err != nil {
		t.Fatalf("insert notification: %v", err)
	}
}
