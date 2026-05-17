// SPDX-License-Identifier: AGPL-3.0-or-later

package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// KindNotifyDigestSweep is the worker job kind. Invoked on a beat by
// the operator's systemd timer (same mechanism as the cron-workflow
// sweep). One job per tick; the handler claims up to DigestSweepBatch
// due rows and re-enqueues itself when a batch fills.
const KindNotifyDigestSweep worker.Kind = "notify:digest_sweep"

// DigestSweepBatch caps the per-tick claim so a backlog can't
// monopolize one worker. Matches the cron-workflow sweep's bound.
const DigestSweepBatch = 100

// DigestSweepDeps wires the sweep handler.
type DigestSweepDeps struct {
	Pool        *pgxpool.Pool
	Logger      *slog.Logger
	EmailSender email.Sender
	EmailFrom   string
	SiteName    string
	BaseURL     string
	// EnforceDigests, when true, skips delivery for recipients whose
	// entitlements forbid FeatureInboxDigests. Off (the default) keeps
	// the sweep running and logs the would-deny — the soak path for
	// PRO-EXT01-17.
	EnforceDigests bool
	// Now is overridable in tests to make NextSendTime deterministic.
	Now func() time.Time
}

func (d DigestSweepDeps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

// SweepOnce claims up to DigestSweepBatch due digest rows, composes
// + sends each one, and advances next_send_at. Returns the number of
// rows processed so the caller can decide to re-enqueue.
//
// Per-row failures are logged but don't stop the sweep — one bad
// recipient (e.g. their primary email vanished) shouldn't block
// every other digest in the batch.
func SweepOnce(ctx context.Context, deps DigestSweepDeps) (int, error) {
	if deps.Pool == nil {
		return 0, errors.New("notifications: SweepOnce needs Pool")
	}
	q := notifdb.New()
	rows, err := q.ClaimDueNotificationDigests(ctx, deps.Pool, DigestSweepBatch)
	if err != nil {
		return 0, fmt.Errorf("digest: claim: %w", err)
	}
	processed := 0
	for _, row := range rows {
		if err := processOne(ctx, deps, q, row); err != nil {
			if deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "digest: send failed",
					"user_id", row.UserID, "error", err)
			}
			continue
		}
		processed++
	}
	return processed, nil
}

// processOne handles a single claimed row: entitlement gate, fetch
// unread notifications, compose + send, advance the cursor.
func processOne(
	ctx context.Context, deps DigestSweepDeps,
	q *notifdb.Queries, row notifdb.UserNotificationDigest,
) error {
	if !digestAllowed(ctx, deps, row.UserID) {
		// Even when blocked we still advance next_send_at so we
		// don't spin on the same row every tick.
		return advance(ctx, deps, q, row)
	}

	user, err := usersdb.New().GetUserByID(ctx, deps.Pool, row.UserID)
	if err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	emailAddr, err := primaryEmailFor(ctx, deps, user.ID)
	if err != nil {
		return fmt.Errorf("primary email: %w", err)
	}
	if emailAddr == "" {
		// No verified primary email → silently skip + advance. The
		// user can re-enable digest after adding an email.
		return advance(ctx, deps, q, row)
	}

	items, err := q.ListUnreadNotificationsForDigest(ctx, deps.Pool, row.UserID)
	if err != nil {
		return fmt.Errorf("list unread: %w", err)
	}
	if len(items) == 0 {
		// Empty inbox — skip sending (would be a useless email) but
		// still advance the cursor.
		return advance(ctx, deps, q, row)
	}

	if deps.EmailSender != nil {
		body := composeDigest(deps, user, items)
		msg := email.Message{
			From:    deps.EmailFrom,
			To:      emailAddr,
			Subject: digestSubject(deps.SiteName, len(items)),
			Text:    body,
		}
		if err := deps.EmailSender.Send(ctx, msg); err != nil {
			return fmt.Errorf("send: %w", err)
		}
	}
	return advance(ctx, deps, q, row)
}

func advance(
	ctx context.Context, deps DigestSweepDeps,
	q *notifdb.Queries, row notifdb.UserNotificationDigest,
) error {
	next := NextSendTime(deps.now(), row.Frequency, int(row.HourUtc), dayOfWeek(row))
	return q.AdvanceUserNotificationDigest(ctx, deps.Pool, notifdb.AdvanceUserNotificationDigestParams{
		UserID:     row.UserID,
		NextSendAt: pgtype.Timestamptz{Time: next, Valid: true},
	})
}

func dayOfWeek(row notifdb.UserNotificationDigest) int {
	if row.DayOfWeek.Valid {
		return int(row.DayOfWeek.Int16)
	}
	return 0
}

func digestAllowed(ctx context.Context, deps DigestSweepDeps, userID int64) bool {
	principal := billing.PrincipalForUser(userID)
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: deps.Pool}, principal, entitlements.FeatureInboxDigests)
	if err != nil {
		return !deps.EnforceDigests
	}
	if decision.Allowed {
		return true
	}
	if deps.Logger != nil {
		mode := "report_only"
		if deps.EnforceDigests {
			mode = "enforce"
		}
		deps.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", principal.String(),
			"principal_kind", string(principal.Kind),
			"principal_id", userID,
			"feature", string(entitlements.FeatureInboxDigests),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "digest-sweep")
	}
	return !deps.EnforceDigests
}

// primaryEmailFor returns the user's verified primary email address
// or "" if none is set. The users table carries a nullable
// primary_email_id; we follow the FK to the user_emails row and
// double-check Verified before returning.
func primaryEmailFor(ctx context.Context, deps DigestSweepDeps, userID int64) (string, error) {
	q := usersdb.New()
	user, err := q.GetUserByID(ctx, deps.Pool, userID)
	if err != nil {
		return "", err
	}
	if !user.PrimaryEmailID.Valid {
		return "", nil
	}
	em, err := q.GetUserEmailByID(ctx, deps.Pool, user.PrimaryEmailID.Int64)
	if err != nil {
		return "", err
	}
	if !em.Verified {
		return "", nil
	}
	return em.Email, nil
}

// NextSendTime computes the next tick from a base instant + cadence.
// Exported so the handler can stamp the right next_send_at on insert
// without re-implementing the math.
//
// Daily: next instance of the hour-of-day at or after base.
// Weekly: next instance of (day-of-week, hour-of-day) at or after base.
//
// Both round forward by exactly one period if base already past the
// candidate — matches the "skip missed ticks" semantic the cron-
// workflow sweep settled on.
func NextSendTime(base time.Time, freq notifdb.UserNotificationDigestFrequency, hourUTC, dayOfWeek int) time.Time {
	base = base.UTC()
	switch freq {
	case notifdb.UserNotificationDigestFrequencyWeekly:
		return nextWeeklyAfter(base, hourUTC, dayOfWeek)
	default: // daily
		return nextDailyAfter(base, hourUTC)
	}
}

func nextDailyAfter(base time.Time, hourUTC int) time.Time {
	candidate := time.Date(base.Year(), base.Month(), base.Day(), hourUTC, 0, 0, 0, time.UTC)
	if !candidate.After(base) {
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}

func nextWeeklyAfter(base time.Time, hourUTC, dayOfWeek int) time.Time {
	candidate := time.Date(base.Year(), base.Month(), base.Day(), hourUTC, 0, 0, 0, time.UTC)
	for {
		if int(candidate.Weekday()) == dayOfWeek && candidate.After(base) {
			return candidate
		}
		candidate = candidate.Add(24 * time.Hour)
	}
}

// composeDigest builds the plain-text body. HTML email is a follow-up
// — text-only is the safer default for transactional bulk where some
// recipients have HTML disabled and the deliverability hit of mixed-
// part bodies isn't worth it on day 1.
func composeDigest(
	deps DigestSweepDeps,
	user usersdb.User,
	items []notifdb.ListUnreadNotificationsForDigestRow,
) string {
	var b strings.Builder
	displayName := user.DisplayName
	if displayName == "" {
		displayName = user.Username
	}
	fmt.Fprintf(&b, "Hi %s,\n\n", displayName)
	fmt.Fprintf(&b, "Your %s digest — %d unread notification%s in the last window.\n\n",
		deps.SiteName, len(items), plural(len(items)))
	for i, it := range items {
		fmt.Fprintf(&b, "%d. ", i+1)
		if it.RepoOwnerUsername != "" && it.RepoName != "" {
			fmt.Fprintf(&b, "[%s/%s] ", it.RepoOwnerUsername, it.RepoName)
		}
		if it.ThreadTitle != "" {
			fmt.Fprintf(&b, "%s ", it.ThreadTitle)
		}
		fmt.Fprintf(&b, "(%s)", it.Reason)
		if it.ActorUsername != "" {
			fmt.Fprintf(&b, " from @%s", it.ActorUsername)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "\nOpen your inbox: %s/notifications\n", strings.TrimRight(deps.BaseURL, "/"))
	fmt.Fprintf(&b, "Manage your digest schedule: %s/settings/notifications\n",
		strings.TrimRight(deps.BaseURL, "/"))
	return b.String()
}

func digestSubject(siteName string, n int) string {
	return fmt.Sprintf("[%s] %d unread notification%s", siteName, n, plural(n))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
