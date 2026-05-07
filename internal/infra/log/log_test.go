// SPDX-License-Identifier: AGPL-3.0-or-later

package log

import (
	"bytes"
	"strings"
	"testing"
)

func TestNew_RedactsSecretKeys(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := New(Options{Level: "debug", Format: "json", Writer: &buf})

	logger.Info(
		"login attempt",
		"username", "alice",
		"password", "hunter2",
		"otp_secret", "JBSWY3DPEHPK3PXP",
		"authorization", "Bearer eyJhbGc",
	)

	out := buf.String()
	for _, leak := range []string{"hunter2", "JBSWY3DPEHPK3PXP", "Bearer eyJhbGc"} {
		if strings.Contains(out, leak) {
			t.Errorf("redaction missed %q\nlog: %s", leak, out)
		}
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("non-secret value lost: %s", out)
	}
}

func TestNew_RedactsSecretValuesByMarker(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := New(Options{Level: "info", Format: "json", Writer: &buf})

	logger.Info("token used", "trace_note", "saw header: Bearer eyJfoo")
	logger.Info("pat seen", "request_path", "/api/repos?token=shithub_pat_abc123")
	logger.Info("totp uri", "url", "otpauth://totp/...")

	out := buf.String()
	for _, leak := range []string{"eyJfoo", "shithub_pat_abc123", "otpauth://"} {
		if strings.Contains(out, leak) {
			t.Errorf("value-marker redaction missed %q\nlog: %s", leak, out)
		}
	}
}

func TestNew_KeepsOrdinaryValues(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := New(Options{Level: "info", Format: "json", Writer: &buf})
	logger.Info("repo created", "owner", "alice", "name", "my-project")
	out := buf.String()
	if !strings.Contains(out, "alice") || !strings.Contains(out, "my-project") {
		t.Errorf("non-secret fields dropped: %s", out)
	}
}
