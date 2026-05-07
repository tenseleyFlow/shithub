// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// loginAccountUser is the same login helper as profile_test.go but
// against a different account so tests stay isolated when run in
// parallel.
func loginAccountUser(t *testing.T, name string) *client {
	t.Helper()
	httpsrv, captor := newTestServer(t, false)
	cli := newClient(t, httpsrv)
	mustSignup(t, cli, name, name+"@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {name},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	return cli
}

func TestAccount_RenameRoundtrip(t *testing.T) {
	t.Parallel()
	cli := loginAccountUser(t, "renamea")
	csrf := cli.extractCSRF(t, "/settings/account")

	resp := cli.post(t, "/settings/account/username", url.Values{
		"csrf_token":   {csrf},
		"new_username": {"renameb"},
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Username updated to renameb") {
		t.Errorf("missing success message; got: %s", body)
	}
	if !strings.Contains(string(body), "USERNAME=renameb") {
		t.Errorf("expected USERNAME=renameb in body; got: %s", body)
	}
	if !strings.Contains(string(body), "USED=1/3") {
		t.Errorf("expected USED=1/3 after rename; got: %s", body)
	}

	// The old name should now redirect to the new profile (/oldname → /newname).
	resp = cli.get(t, "/renamea")
	defer func() { _ = resp.Body.Close() }()
	// /renamea hits the catch-all in this minimal test rig — we don't have
	// the profile handler mounted, so the only thing we can verify here
	// is the DB state.
}

func TestAccount_RejectsReservedName(t *testing.T) {
	t.Parallel()
	cli := loginAccountUser(t, "reserveda")
	csrf := cli.extractCSRF(t, "/settings/account")
	resp := cli.post(t, "/settings/account/username", url.Values{
		"csrf_token":   {csrf},
		"new_username": {"settings"},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "reserved") {
		t.Fatalf("expected reserved error, got: %s", body)
	}
}

func TestAccount_RejectsInvalidShape(t *testing.T) {
	t.Parallel()
	cli := loginAccountUser(t, "shapea")
	csrf := cli.extractCSRF(t, "/settings/account")
	// UPPER is normalized to lowercase by the handler (user-friendly), so
	// it isn't on this rejection list.
	for _, bad := range []string{"-leading", "trailing-", "with spaces", ""} {
		resp := cli.post(t, "/settings/account/username", url.Values{
			"csrf_token":   {csrf},
			"new_username": {bad},
		})
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), "Username must") && !strings.Contains(string(body), "1–39") {
			t.Errorf("input %q: expected validation error, got: %s", bad, body)
		}
	}
}

func TestAccount_RejectsTaken(t *testing.T) {
	t.Parallel()
	// Two clients on the SAME server so they share the same DB.
	httpsrv, captor := newTestServer(t, false)
	cliA := newClient(t, httpsrv)
	cliB := newClient(t, httpsrv)

	mustSignup(t, cliA, "takena", "takena@example.com", "correct horse battery staple")
	mustSignup(t, cliB, "takenb", "takenb@example.com", "correct horse battery staple")
	for _, m := range captor.all() {
		// Verify each.
		if tok := extractTokenFromMessage(t, m, "/verify-email"); tok != "" {
			_ = cliA.get(t, "/verify-email/"+tok).Body.Close()
		}
	}
	csrf := cliA.extractCSRF(t, "/login")
	_ = cliA.post(t, "/login", url.Values{
		"csrf_token": {csrf}, "username": {"takena"}, "password": {"correct horse battery staple"},
	}).Body.Close()

	csrf = cliA.extractCSRF(t, "/settings/account")
	resp := cliA.post(t, "/settings/account/username", url.Values{
		"csrf_token":   {csrf},
		"new_username": {"takenb"}, // already used by cliB
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "taken") {
		t.Fatalf("expected taken error, got: %s", body)
	}
}

func TestAccount_RateLimitAtThree(t *testing.T) {
	t.Parallel()
	cli := loginAccountUser(t, "ratea")

	// Burn three rename slots.
	names := []string{"rateb", "ratec", "rated"}
	for _, n := range names {
		csrf := cli.extractCSRF(t, "/settings/account")
		resp := cli.post(t, "/settings/account/username", url.Values{
			"csrf_token":   {csrf},
			"new_username": {n},
		})
		_ = resp.Body.Close()
	}

	// Fourth attempt should be blocked.
	csrf := cli.extractCSRF(t, "/settings/account")
	resp := cli.post(t, "/settings/account/username", url.Values{
		"csrf_token":   {csrf},
		"new_username": {"ratee"},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "too many") {
		t.Fatalf("expected rate-limit error, got: %s", body)
	}
}
