// SPDX-License-Identifier: AGPL-3.0-or-later

// Package email is the transactional-email layer for the auth surface.
//
// Concrete implementations:
//   - Stdout — writes a human-readable dump of every email to a writer.
//     Used in tests and as the dev default when no SMTP is configured.
//   - MailHog (any plain SMTP server) — used by `make dev-email` for
//     local end-to-end testing.
//   - Postmark — the prod implementation. Skeleton lives in postmark.go;
//     full credentials wired by S35 deploy work.
//
// Templates live alongside the auth handlers under templates/email/ and
// have HTML + plaintext variants. Senders MUST send both.
package email

import (
	"context"
	"fmt"
	"io"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

// Message is the unit a Sender ships. Both HTML and Text are required —
// every transactional email shithub sends must work in plain-text clients.
type Message struct {
	From    string
	To      string
	Subject string
	HTML    string
	Text    string
}

// Sender is the abstract interface every backend implements.
// Implementations MUST be safe for concurrent use.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// StdoutSender writes a dump of every send to w. Convenient for tests
// (capture w to assert) and as the dev default when no real backend is
// configured. Implements Sender.
type StdoutSender struct {
	mu sync.Mutex
	w  io.Writer
}

// NewStdoutSender returns a sender that writes to w. Pass os.Stdout in dev.
func NewStdoutSender(w io.Writer) *StdoutSender { return &StdoutSender{w: w} }

// Send implements Sender.
func (s *StdoutSender) Send(_ context.Context, m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := fmt.Fprintf(s.w,
		"--- email (%s) ---\nFrom: %s\nTo: %s\nSubject: %s\n\n[text]\n%s\n\n[html]\n%s\n--- end ---\n",
		time.Now().UTC().Format(time.RFC3339), m.From, m.To, m.Subject, m.Text, m.HTML)
	return err
}

// SMTPSender ships messages through a plain SMTP server. Used for
// MailHog locally. SMTP credentials are optional (MailHog accepts none).
type SMTPSender struct {
	Addr     string // host:port
	From     string // default From when Message.From is empty
	Username string // optional
	Password string // optional
	UseTLS   bool   // STARTTLS upgrade after EHLO
}

// Send implements Sender. Builds a multipart/alternative body so MUAs can
// pick the variant they prefer.
//
// The SMTP envelope sender is the bare address — `MAIL FROM:<...>` per
// RFC 5321 forbids nested angle brackets, so a configured `From` like
// `shithub <noreply@shithub.local>` only goes in the message header.
// The displayed-name form lives unchanged in the `From:` header inside
// the body.
func (s *SMTPSender) Send(_ context.Context, m Message) error {
	if m.From == "" {
		m.From = s.From
	}
	body := buildMultipart(m)

	var auth smtp.Auth
	if s.Username != "" {
		host := s.Addr
		if i := strings.IndexByte(host, ':'); i > 0 {
			host = host[:i]
		}
		auth = smtp.PlainAuth("", s.Username, s.Password, host)
	}
	return smtp.SendMail(s.Addr, auth, envelopeAddress(m.From), []string{m.To}, body)
}

// envelopeAddress extracts the bare email address from a possibly-
// displayed-name form like `Name <addr@example>`. Returns the input
// unchanged when no angle brackets are present (already bare).
func envelopeAddress(from string) string {
	if i := strings.IndexByte(from, '<'); i >= 0 {
		if j := strings.IndexByte(from[i+1:], '>'); j >= 0 {
			return from[i+1 : i+1+j]
		}
	}
	return from
}

// boundary is a fixed multipart boundary. Per RFC 2046 the boundary need
// only be unique within a message; a constant is fine here because we
// always include the canonical Content-Type header that names it.
const boundary = "shithub-mime-boundary-x"

func buildMultipart(m Message) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", m.From)
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", m.Subject)
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)

	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", boundary, m.Text)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n", boundary, m.HTML)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)

	return []byte(b.String())
}
