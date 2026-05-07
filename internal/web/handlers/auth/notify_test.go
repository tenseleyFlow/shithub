// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
)

// loginNotifTest runs the signup → verify → login flow and returns the
// authenticated client plus the email captor so tests can assert what
// was (or wasn't) sent.
func loginNotifTest(t *testing.T, name, password string) (*client, *captureSender) {
	t.Helper()
	httpsrv, captor := newTestServer(t, false)
	cli := newClient(t, httpsrv)
	mustSignup(t, cli, name, name+"@example.com", password)
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {name},
		"password":   {password},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	return cli, captor
}

// findSubjectContaining returns the first captured Message whose Subject
// contains substr. Each state-change email subject is distinct.
func findSubjectContaining(msgs []email.Message, substr string) *email.Message {
	for i := range msgs {
		if strings.Contains(msgs[i].Subject, substr) {
			return &msgs[i]
		}
	}
	return nil
}

func TestNotify_PasswordChangeSendsEmail(t *testing.T) {
	t.Parallel()
	cli, captor := loginNotifTest(t, "npa", pwOriginal)
	captor.reset()

	csrf := cli.extractCSRF(t, "/settings/password")
	_ = cli.post(t, "/settings/password", url.Values{
		"csrf_token":       {csrf},
		"current_password": {pwOriginal},
		"new_password":     {pwNew},
		"confirm_password": {pwNew},
	}).Body.Close()

	if findSubjectContaining(captor.all(), "password was changed") == nil {
		t.Fatalf("expected password_changed email; got %d msgs", len(captor.all()))
	}
}

func TestNotify_UsernameChangeSendsEmail(t *testing.T) {
	t.Parallel()
	cli, captor := loginNotifTest(t, "nub", pwOriginal)
	captor.reset()

	csrf := cli.extractCSRF(t, "/settings/account")
	_ = cli.post(t, "/settings/account/username", url.Values{
		"csrf_token":   {csrf},
		"new_username": {"nub-renamed"},
	}).Body.Close()

	if findSubjectContaining(captor.all(), "username was changed") == nil {
		t.Fatalf("expected username_changed email; got %d msgs", len(captor.all()))
	}
}

func TestNotify_LogoutEverywhereSendsEmail(t *testing.T) {
	t.Parallel()
	cli, captor := loginNotifTest(t, "nle", pwOriginal)
	captor.reset()

	csrf := cli.extractCSRF(t, "/settings/sessions")
	_ = cli.post(t, "/settings/sessions/logout-everywhere", url.Values{
		"csrf_token": {csrf},
	}).Body.Close()

	if findSubjectContaining(captor.all(), "All other sessions") == nil {
		t.Fatalf("expected log_out_everywhere email; got %d msgs", len(captor.all()))
	}
}

func TestNotify_DeletionSendsEmail(t *testing.T) {
	t.Parallel()
	cli, captor := loginNotifTest(t, "ndd", pwOriginal)
	captor.reset()

	csrf := cli.extractCSRF(t, "/settings/danger")
	resp := cli.post(t, "/settings/danger", url.Values{
		"csrf_token":       {csrf},
		"confirm_username": {"ndd"},
		"password":         {pwOriginal},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if findSubjectContaining(captor.all(), "scheduled for deletion") == nil {
		t.Fatalf("expected account_deletion_initiated email; got %d msgs", len(captor.all()))
	}
}

func TestNotify_AccountChangeOptOutSuppresses(t *testing.T) {
	t.Parallel()
	cli, captor := loginNotifTest(t, "nopt", pwOriginal)

	// Opt out of account_changes.
	csrf := cli.extractCSRF(t, "/settings/notifications")
	_ = cli.post(t, "/settings/notifications", url.Values{
		"csrf_token": {csrf},
		// account_changes intentionally omitted
	}).Body.Close()

	captor.reset()

	// Username change is on the account_changes channel — should NOT
	// produce an email now.
	csrf = cli.extractCSRF(t, "/settings/account")
	_ = cli.post(t, "/settings/account/username", url.Values{
		"csrf_token":   {csrf},
		"new_username": {"nopt-renamed"},
	}).Body.Close()

	if findSubjectContaining(captor.all(), "username was changed") != nil {
		t.Fatalf("expected NO username email after opt-out; got %d msgs", len(captor.all()))
	}
}

func TestNotify_SecurityAlertNeverSuppressed(t *testing.T) {
	t.Parallel()
	cli, captor := loginNotifTest(t, "nopt2", pwOriginal)

	// Opt out of account_changes (security_alerts is required so it
	// can't be opted out of).
	csrf := cli.extractCSRF(t, "/settings/notifications")
	_ = cli.post(t, "/settings/notifications", url.Values{
		"csrf_token": {csrf},
	}).Body.Close()

	captor.reset()

	csrf = cli.extractCSRF(t, "/settings/password")
	_ = cli.post(t, "/settings/password", url.Values{
		"csrf_token":       {csrf},
		"current_password": {pwOriginal},
		"new_password":     {pwNew},
		"confirm_password": {pwNew},
	}).Body.Close()

	if findSubjectContaining(captor.all(), "password was changed") == nil {
		t.Fatalf("password change should always notify regardless of prefs; got %d msgs", len(captor.all()))
	}
}
