// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestEnvelopeAddress_StripsDisplayName(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"shithub <noreply@shithub.local>", "noreply@shithub.local"},
		{"<bare@example.com>", "bare@example.com"},
		{"plain@example.com", "plain@example.com"},
		{"Name With Spaces <a@b>", "a@b"},
		{"malformed <still works", "malformed <still works"},
	}
	for _, c := range cases {
		if got := envelopeAddress(c.in); got != c.want {
			t.Errorf("envelopeAddress(%q): want %q, got %q", c.in, c.want, got)
		}
	}
}

func TestStdoutSender_WritesBothBodies(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	s := NewStdoutSender(&buf)
	if err := s.Send(context.Background(), Message{
		From: "noreply@x", To: "alice@x", Subject: "hi",
		HTML: "<b>hi</b>", Text: "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"From: noreply@x", "To: alice@x", "Subject: hi", "[text]", "[html]", "<b>hi</b>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestVerifyMessage_RendersBothBodies(t *testing.T) {
	t.Parallel()
	b := Branding{SiteName: "shithub", BaseURL: "https://example", From: "noreply@example"}
	m, err := VerifyMessage(b, "alice@example", "alice", "TOKEN-XYZ")
	if err != nil {
		t.Fatalf("VerifyMessage: %v", err)
	}
	for _, body := range []string{m.HTML, m.Text} {
		if !strings.Contains(body, "https://example/verify-email/TOKEN-XYZ") {
			t.Errorf("verify body missing link: %s", body)
		}
		if !strings.Contains(body, "shithub") {
			t.Errorf("verify body missing site name")
		}
	}
	if m.Subject == "" || m.From != "noreply@example" || m.To != "alice@example" {
		t.Fatalf("envelope wrong: %+v", m)
	}
}

func TestResetMessage_RendersBothBodies(t *testing.T) {
	t.Parallel()
	b := Branding{SiteName: "shithub", BaseURL: "https://example", From: "noreply@example"}
	m, err := ResetMessage(b, "alice@example", "T")
	if err != nil {
		t.Fatalf("ResetMessage: %v", err)
	}
	for _, body := range []string{m.HTML, m.Text} {
		if !strings.Contains(body, "https://example/password/reset/T") {
			t.Errorf("reset body missing link: %s", body)
		}
	}
}

func TestBuildMultipart_StructureSane(t *testing.T) {
	t.Parallel()
	body := buildMultipart(Message{From: "f", To: "t", Subject: "s", HTML: "H", Text: "T"})
	out := string(body)
	for _, want := range []string{
		"From: f", "To: t", "Subject: s",
		"multipart/alternative", "text/plain", "text/html",
		"H", "T",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
