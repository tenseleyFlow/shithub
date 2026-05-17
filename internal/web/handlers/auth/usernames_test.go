// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestUsernameReservations_FreeBlockedProAllowed confirms the gate.
// Free user POSTing the form gets the upgrade banner and no row is
// written; Pro user POSTing gets a success message + the row.
func TestUsernameReservations_FreeBlockedProAllowed(t *testing.T) {
	t.Parallel()
	// PRO-EXT_SR2-09 added the enforce knob; flip it on so the test
	// keeps asserting hard-deny for Free. Report-only path is covered
	// separately by the entitlement-level tests.
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceUsernameReservations: true,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freealice")

	// Free attempt — gate denies.
	csrf := cli.extractCSRF(t, "/settings/usernames")
	resp := cli.post(t, "/settings/usernames", url.Values{
		"csrf_token": {csrf},
		"handle":     {"alice-future"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "ALLOW=false") {
		t.Fatalf("Free user should see ALLOW=false: %s", body)
	}
	if !strings.Contains(string(body), "Upgrade") {
		t.Fatalf("Free user should see upgrade message: %s", body)
	}
	assertReservationCount(t, pool, userID, 0)

	// Upgrade to Pro and retry.
	upgradeProfileTestUserToPro(t, pool, userID)
	csrf = cli.extractCSRF(t, "/settings/usernames")
	resp = cli.post(t, "/settings/usernames", url.Values{
		"csrf_token": {csrf},
		"handle":     {"alice-future"},
	})
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Reserved alice-future") {
		t.Fatalf("Pro POST should succeed: %s", body)
	}
	if !strings.Contains(string(body), "USED=1/3") {
		t.Fatalf("USED counter not incremented: %s", body)
	}
	assertReservationCount(t, pool, userID, 1)
}

// TestUsernameReservations_CapEnforced confirms the Pro cap of 3.
// Fourth attempt is rejected with a clear message.
func TestUsernameReservations_CapEnforced(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "proalice")
	upgradeProfileTestUserToPro(t, pool, userID)

	for i := 0; i < 3; i++ {
		csrf := cli.extractCSRF(t, "/settings/usernames")
		resp := cli.post(t, "/settings/usernames", url.Values{
			"csrf_token": {csrf},
			"handle":     {"reservation-" + strconv.Itoa(i)},
		})
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), "Reserved reservation-") {
			t.Fatalf("reservation %d should succeed: %s", i, body)
		}
	}

	// Fourth attempt.
	csrf := cli.extractCSRF(t, "/settings/usernames")
	resp := cli.post(t, "/settings/usernames", url.Values{
		"csrf_token": {csrf},
		"handle":     {"reservation-too-many"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "maximum") {
		t.Fatalf("4th reservation should hit cap: %s", body)
	}
	assertReservationCount(t, pool, userID, 3)
}

// TestUsernameReservations_BlocksSignup confirms a reserved handle
// fails a signup attempt by another user.
func TestUsernameReservations_BlocksSignup(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cliReserver, userID := newBillingTestUser(t, srv, pool, captor, "proalice")
	upgradeProfileTestUserToPro(t, pool, userID)

	csrf := cliReserver.extractCSRF(t, "/settings/usernames")
	resp := cliReserver.post(t, "/settings/usernames", url.Values{
		"csrf_token": {csrf},
		"handle":     {"alice-future"},
	})
	_ = resp.Body.Close()
	assertReservationCount(t, pool, userID, 1)

	// Different client, fresh user attempts to sign up with the
	// reserved handle.
	cliSquatter := newClient(t, srv)
	csrf = cliSquatter.extractCSRF(t, "/signup")
	resp = cliSquatter.post(t, "/signup", url.Values{
		"csrf_token": {csrf},
		"username":   {"alice-future"},
		"email":      {"squatter@example.com"},
		"password":   {"correct horse battery staple"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "reserved") {
		t.Fatalf("signup should be blocked by reservation: %s", body)
	}
}

// TestUsernameReservations_BlocksRename confirms a Free user's rename
// to a reserved handle fails. The Free user can still rename to other
// available names.
func TestUsernameReservations_BlocksRename(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cliReserver, userID := newBillingTestUser(t, srv, pool, captor, "proreserver")
	upgradeProfileTestUserToPro(t, pool, userID)

	csrf := cliReserver.extractCSRF(t, "/settings/usernames")
	resp := cliReserver.post(t, "/settings/usernames", url.Values{
		"csrf_token": {csrf},
		"handle":     {"alice-future"},
	})
	_ = resp.Body.Close()

	// A Free user signs up + tries to rename to the reserved handle.
	cliFree := newClient(t, srv)
	csrf = cliFree.extractCSRF(t, "/signup")
	resp = cliFree.post(t, "/signup", url.Values{
		"csrf_token": {csrf},
		"username":   {"freebob"},
		"email":      {"freebob@example.com"},
		"password":   {"correct horse battery staple"},
	})
	_ = resp.Body.Close()
	// Sign in as the new free user.
	tok := extractTokenFromMessage(t, captor.all()[len(captor.all())-1], "/verify-email")
	_ = cliFree.get(t, "/verify-email/"+tok).Body.Close()
	csrf = cliFree.extractCSRF(t, "/login")
	resp = cliFree.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"freebob"},
		"password":   {"correct horse battery staple"},
	})
	_ = resp.Body.Close()

	csrf = cliFree.extractCSRF(t, "/settings/account")
	resp = cliFree.post(t, "/settings/account/username", url.Values{
		"csrf_token":   {csrf},
		"new_username": {"alice-future"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "reserved") {
		t.Fatalf("rename should be blocked by reservation: %s", body)
	}
}

// TestUsernameReservations_ReleaseRemovesRow confirms the release form
// drops a single reservation by id without affecting others.
func TestUsernameReservations_ReleaseRemovesRow(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "proalice")
	upgradeProfileTestUserToPro(t, pool, userID)

	csrf := cli.extractCSRF(t, "/settings/usernames")
	resp := cli.post(t, "/settings/usernames", url.Values{
		"csrf_token": {csrf},
		"handle":     {"keep-me"},
	})
	_ = resp.Body.Close()
	csrf = cli.extractCSRF(t, "/settings/usernames")
	resp = cli.post(t, "/settings/usernames", url.Values{
		"csrf_token": {csrf},
		"handle":     {"drop-me"},
	})
	_ = resp.Body.Close()
	assertReservationCount(t, pool, userID, 2)

	var dropID int64
	if err := pool.QueryRow(
		context.Background(),
		`SELECT id FROM user_username_reservations WHERE user_id = $1 AND reserved_handle = $2`,
		userID, "drop-me",
	).Scan(&dropID); err != nil {
		t.Fatalf("lookup drop reservation id: %v", err)
	}

	csrf = cli.extractCSRF(t, "/settings/usernames")
	resp = cli.post(t, "/settings/usernames/release", url.Values{
		"csrf_token":     {csrf},
		"reservation_id": {strconv.FormatInt(dropID, 10)},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "released") {
		t.Fatalf("release should succeed: %s", body)
	}
	assertReservationCount(t, pool, userID, 1)
	var remaining string
	if err := pool.QueryRow(
		context.Background(),
		`SELECT reserved_handle FROM user_username_reservations WHERE user_id = $1`,
		userID,
	).Scan(&remaining); err != nil {
		t.Fatalf("read remaining: %v", err)
	}
	if remaining != "keep-me" {
		t.Errorf("released wrong row: %q remains", remaining)
	}
}

// TestUsernameReservations_DuplicateBlocked confirms two Pro users
// cannot reserve the same handle.
func TestUsernameReservations_DuplicateBlocked(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cliA, userA := newBillingTestUser(t, srv, pool, captor, "proalice")
	upgradeProfileTestUserToPro(t, pool, userA)
	csrf := cliA.extractCSRF(t, "/settings/usernames")
	resp := cliA.post(t, "/settings/usernames", url.Values{
		"csrf_token": {csrf},
		"handle":     {"shared-handle"},
	})
	_ = resp.Body.Close()

	cliB := newClient(t, srv)
	mustSignup(t, cliB, "probob", "probob@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[len(captor.all())-1], "/verify-email")
	_ = cliB.get(t, "/verify-email/"+tok).Body.Close()
	csrf = cliB.extractCSRF(t, "/login")
	resp = cliB.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"probob"},
		"password":   {"correct horse battery staple"},
	})
	_ = resp.Body.Close()
	var userB int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE username = $1`, "probob").Scan(&userB); err != nil {
		t.Fatalf("lookup user B id: %v", err)
	}
	upgradeProfileTestUserToPro(t, pool, userB)

	csrf = cliB.extractCSRF(t, "/settings/usernames")
	resp = cliB.post(t, "/settings/usernames", url.Values{
		"csrf_token": {csrf},
		"handle":     {"shared-handle"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "reserved by another") && !strings.Contains(string(body), "already reserved") {
		t.Fatalf("Pro user B should be blocked from duplicating reservation: %s", body)
	}
}

func assertReservationCount(t *testing.T, pool *pgxpool.Pool, userID int64, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM user_username_reservations WHERE user_id = $1`, userID,
	).Scan(&got); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if got != want {
		t.Errorf("reservation count: got %d, want %d", got, want)
	}
}
