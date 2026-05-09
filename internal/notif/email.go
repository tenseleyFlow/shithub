// SPDX-License-Identifier: AGPL-3.0-or-later

package notif

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// sendNotificationEmail renders + sends one notification email and
// records it in notification_email_log. Idempotency is the
// dampener's job, not ours; we send what we're told to send.
//
// The email body is intentionally minimal in v1: a plaintext line
// summarising the reason + a link to the thread. Per-kind HTML
// templates land in a follow-up commit (or the next sprint that
// touches the email surface). The List-Unsubscribe URL is wired
// up so the standards-compliant bits (RFC 8058) are in place from
// day one.
func sendNotificationEmail(
	ctx context.Context, deps Deps,
	recipientID int64,
	notif notifdb.Notification,
	reason Reason,
	threadKind notifdb.NullNotificationThreadKind,
	threadID int64,
) error {
	if deps.EmailSender == nil {
		return nil
	}
	uq := usersdb.New()
	user, err := uq.GetUserByID(ctx, deps.Pool, recipientID)
	if err != nil {
		return fmt.Errorf("load recipient: %w", err)
	}
	if !user.PrimaryEmailID.Valid {
		// No verified primary; nothing to send to.
		return nil
	}
	em, err := uq.GetUserEmailByID(ctx, deps.Pool, user.PrimaryEmailID.Int64)
	if err != nil || !em.Verified {
		return nil
	}

	threadURL := buildThreadURL(deps.BaseURL, ctx, deps.Pool, notif)
	subject := buildSubject(deps.SiteName, notif, reason)
	plain := buildPlainBody(deps.SiteName, notif, reason, threadURL)
	htmlBody := buildHTMLBody(deps.SiteName, notif, reason, threadURL)

	// The List-Unsubscribe URL is computed but not yet wired into
	// the Sender — the existing email.Message shape (S05) doesn't
	// carry arbitrary headers. Adding a `Headers map[string]string`
	// is a follow-up that ripples through the SMTP / Postmark
	// backends; tracked in the S29 status block as deferred. The
	// URL is still useful as a body-footer fallback.
	unsubURL := unsubscribeURL(deps.BaseURL, deps.UnsubscribeKey, recipientID,
		threadKind.Valid, threadKindString(threadKind), threadID)
	plain += "\nUnsubscribe: " + unsubURL + "\n"

	if err := deps.EmailSender.Send(ctx, email.Message{
		From:    deps.EmailFrom,
		To:      string(em.Email),
		Subject: subject,
		HTML:    htmlBody,
		Text:    plain,
	}); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	// Log the send so the dampener can count it next time.
	if err := notifdb.New().InsertEmailLog(ctx, deps.Pool, notifdb.InsertEmailLogParams{
		RecipientUserID: recipientID,
		NotificationID:  pgInt(notif.ID),
		ThreadKind:      threadKind,
		ThreadID:        pgInt(threadID),
		MessageID:       pgString(""),
	}); err != nil {
		// Non-fatal — the email already went out. Log loudly so
		// the operator notices a logging-side regression.
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "email log insert failed",
				"recipient", recipientID, "error", err)
		}
	}
	return nil
}

// emailPrefOn reads the user's per-key boolean preference. Defaults
// to true when no row exists (matches the spec's "Email all
// participating threads master toggle, default on").
func emailPrefOn(ctx context.Context, pool *pgxpool.Pool, userID int64, key string) (bool, error) {
	var raw json.RawMessage
	err := pool.QueryRow(
		ctx,
		`SELECT value FROM user_notification_prefs WHERE user_id = $1 AND key = $2`,
		userID, key,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		return true, err
	}
	// Stored as JSON `true` or `false`.
	s := strings.TrimSpace(string(raw))
	return s == "true", nil
}

// unsubscribeURL builds an HMAC-signed one-click unsubscribe link.
// The URL embeds (recipient_id, thread_kind, thread_id, sig) so
// the handler can verify and act without a session cookie. Key
// rotation invalidates outstanding links — operators are expected
// to keep the key stable.
func unsubscribeURL(baseURL string, key []byte, recipientID int64, hasThread bool, threadKind string, threadID int64) string {
	if !hasThread {
		// No thread → can't unsubscribe at the per-thread level.
		// The footer's link should point at the user's email-prefs
		// page instead. Return that path; the email-template
		// renderer can swap it in.
		return strings.TrimRight(baseURL, "/") + "/settings/notifications"
	}
	rec := strconv.FormatInt(recipientID, 10)
	tid := strconv.FormatInt(threadID, 10)
	payload := rec + ":" + threadKind + ":" + tid
	sig := HMACSig(key, payload)
	return strings.TrimRight(baseURL, "/") + "/notifications/unsubscribe?u=" + rec +
		"&tk=" + threadKind + "&ti=" + tid + "&sig=" + sig
}

// VerifyUnsubscribe checks the HMAC. Returns true when the
// signature matches the supplied (recipient, thread) tuple.
func VerifyUnsubscribe(key []byte, recipientID int64, threadKind string, threadID int64, sig string) bool {
	rec := strconv.FormatInt(recipientID, 10)
	tid := strconv.FormatInt(threadID, 10)
	payload := rec + ":" + threadKind + ":" + tid
	want := HMACSig(key, payload)
	return hmac.Equal([]byte(sig), []byte(want))
}

// HMACSig signs a payload with the unsubscribe HMAC key. Exposed so
// tests can build verifying signatures without re-implementing the
// canonical wire format.
func HMACSig(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func threadKindString(k notifdb.NullNotificationThreadKind) string {
	if !k.Valid {
		return ""
	}
	return string(k.NotificationThreadKind)
}

func buildSubject(site string, notif notifdb.Notification, reason Reason) string {
	prefix := site
	if prefix == "" {
		prefix = "shithub"
	}
	return "[" + prefix + "] " + string(reason) + ": " + notif.Kind
}

func buildPlainBody(site string, notif notifdb.Notification, reason Reason, link string) string {
	if site == "" {
		site = "shithub"
	}
	var b strings.Builder
	b.WriteString("You're being notified on " + site + ".\n\n")
	b.WriteString("Reason: " + string(reason) + "\n")
	b.WriteString("Event: " + notif.Kind + "\n")
	if link != "" {
		b.WriteString("\n" + link + "\n")
	}
	b.WriteString("\n— " + site + "\n")
	return b.String()
}

// buildHTMLBody is the deliberately-minimal HTML mirror of the plain
// body. Per-kind templates land in a follow-up; today the layout is
// site / reason / event / link, all escaped. No CSS, no images — most
// MUAs render this fine and the simpler the markup, the lower the
// spam-score risk.
func buildHTMLBody(site string, notif notifdb.Notification, reason Reason, link string) string {
	if site == "" {
		site = "shithub"
	}
	var b strings.Builder
	b.WriteString("<p>You're being notified on ")
	b.WriteString(html.EscapeString(site))
	b.WriteString(".</p>\n")
	b.WriteString("<p><strong>Reason:</strong> ")
	b.WriteString(html.EscapeString(string(reason)))
	b.WriteString("<br><strong>Event:</strong> ")
	b.WriteString(html.EscapeString(notif.Kind))
	b.WriteString("</p>\n")
	if link != "" {
		b.WriteString(`<p><a href="`)
		b.WriteString(html.EscapeString(link))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(link))
		b.WriteString("</a></p>\n")
	}
	b.WriteString("<p>— ")
	b.WriteString(html.EscapeString(site))
	b.WriteString("</p>\n")
	return b.String()
}

// buildThreadURL composes a canonical URL for the thread the
// notification points at. Best-effort: when we can't resolve the
// repo / number we fall back to the notification's inbox row URL.
func buildThreadURL(baseURL string, ctx context.Context, pool *pgxpool.Pool, notif notifdb.Notification) string {
	if !notif.RepoID.Valid || !notif.ThreadID.Valid || !notif.ThreadKind.Valid {
		return strings.TrimRight(baseURL, "/") + "/notifications"
	}
	row, err := notifdb.New().GetNotification(ctx, pool, notif.ID)
	_ = row
	if err != nil {
		return strings.TrimRight(baseURL, "/") + "/notifications"
	}
	// Resolve owner + repo + number with a small ad-hoc query.
	var ownerName, repoName string
	var number int64
	err = pool.QueryRow(ctx, `
		SELECT u.username, r.name, i.number
		FROM repos r
		JOIN users u ON u.id = r.owner_user_id
		LEFT JOIN issues i ON i.id = $1
		WHERE r.id = $2
	`, notif.ThreadID.Int64, notif.RepoID.Int64).Scan(&ownerName, &repoName, &number)
	if err != nil || number == 0 {
		return strings.TrimRight(baseURL, "/") + "/notifications"
	}
	segment := "issues"
	if notif.ThreadKind.NotificationThreadKind == notifdb.NotificationThreadKindPr {
		segment = "pulls"
	}
	return strings.TrimRight(baseURL, "/") + "/" + ownerName + "/" + repoName + "/" + segment + "/" + strconv.FormatInt(number, 10)
}

func pgInt(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: v != 0} }
func pgString(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}
