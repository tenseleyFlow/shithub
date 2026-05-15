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

// PRO-EXT01-07a — settings → saved replies CRUD + Free cap.

// TestSavedReplies_FreeCanUseUpToFreeCap confirms a Free user can add
// FreeSavedRepliesCap (3) replies without seeing the upgrade banner.
// The cap behaviour is exercised by the cap-enforcement test below.
func TestSavedReplies_FreeCanUseUpToFreeCap(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freealice")

	for i := 0; i < 3; i++ {
		csrf := cli.extractCSRF(t, "/settings/saved-replies")
		resp := cli.post(t, "/settings/saved-replies", url.Values{
			"csrf_token": {csrf},
			"name":       {"reply-" + strconv.Itoa(i)},
			"body":       {"body " + strconv.Itoa(i)},
		})
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), "Saved reply added") {
			t.Fatalf("reply %d should succeed: %s", i, body)
		}
	}
	assertSavedRepliesCount(t, pool, userID, 3)
}

// TestSavedReplies_AtFreeCapShowsUpgradeAffordance confirms a Free user
// at the cap sees AT_FREE_CAP=true in the page — the locked-UI signal
// that drives the disabled "Add another" button.
func TestSavedReplies_AtFreeCapShowsUpgradeAffordance(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, _ := newBillingTestUser(t, srv, pool, captor, "freebob")
	for i := 0; i < 3; i++ {
		csrf := cli.extractCSRF(t, "/settings/saved-replies")
		cli.post(t, "/settings/saved-replies", url.Values{
			"csrf_token": {csrf},
			"name":       {"reply-" + strconv.Itoa(i)},
			"body":       {"body " + strconv.Itoa(i)},
		}).Body.Close()
	}

	resp := cli.get(t, "/settings/saved-replies")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "AT_FREE_CAP=true") {
		t.Fatalf("Free user at cap should see AT_FREE_CAP=true: %s", body)
	}
}

// TestSavedReplies_ProAcceptsBeyondFreeCap confirms a Pro user can
// continue creating replies past the Free cap.
func TestSavedReplies_ProAcceptsBeyondFreeCap(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "proalice")
	upgradeProfileTestUserToPro(t, pool, userID)

	for i := 0; i < 5; i++ {
		csrf := cli.extractCSRF(t, "/settings/saved-replies")
		resp := cli.post(t, "/settings/saved-replies", url.Values{
			"csrf_token": {csrf},
			"name":       {"reply-" + strconv.Itoa(i)},
			"body":       {"body " + strconv.Itoa(i)},
		})
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), "Saved reply added") {
			t.Fatalf("Pro reply %d should succeed: %s", i, body)
		}
	}
	assertSavedRepliesCount(t, pool, userID, 5)
}

// TestSavedReplies_EnforceFlipBlocksFreeAtCap exercises the enforce
// flag: with UserSavedRepliesUnlimited=true the 4th attempt by a Free
// user returns the upgrade banner instead of letting the row land.
func TestSavedReplies_EnforceFlipBlocksFreeAtCap(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceSavedRepliesUnlimited: true,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freecarl")

	for i := 0; i < 3; i++ {
		csrf := cli.extractCSRF(t, "/settings/saved-replies")
		cli.post(t, "/settings/saved-replies", url.Values{
			"csrf_token": {csrf},
			"name":       {"reply-" + strconv.Itoa(i)},
			"body":       {"body " + strconv.Itoa(i)},
		}).Body.Close()
	}

	csrf := cli.extractCSRF(t, "/settings/saved-replies")
	resp := cli.post(t, "/settings/saved-replies", url.Values{
		"csrf_token": {csrf},
		"name":       {"reply-too-many"},
		"body":       {"shouldn't land"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Upgrade") {
		t.Fatalf("enforce flip should surface upgrade copy: %s", body)
	}
	assertSavedRepliesCount(t, pool, userID, 3)
}

// TestSavedReplies_ReportOnlyAllowsBeyondFreeCap pins the default
// report-only behaviour: a Free user past the cap is *not* hard-blocked.
// PRO-EXT01-17 flips the enforce flag after the standard soak.
func TestSavedReplies_ReportOnlyAllowsBeyondFreeCap(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freedan")

	for i := 0; i < 5; i++ {
		csrf := cli.extractCSRF(t, "/settings/saved-replies")
		resp := cli.post(t, "/settings/saved-replies", url.Values{
			"csrf_token": {csrf},
			"name":       {"reply-" + strconv.Itoa(i)},
			"body":       {"body " + strconv.Itoa(i)},
		})
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), "Saved reply added") {
			t.Fatalf("report-only reply %d should succeed: %s", i, body)
		}
	}
	assertSavedRepliesCount(t, pool, userID, 5)
}

// TestSavedReplies_UpdateAndDelete rounds out the CRUD: edit name+body
// and then delete the row. Authorization scoping (user_id in the WHERE
// clause) is exercised by the migration's REFERENCES + the SQL queries
// themselves.
func TestSavedReplies_UpdateAndDelete(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freeed")

	csrf := cli.extractCSRF(t, "/settings/saved-replies")
	cli.post(t, "/settings/saved-replies", url.Values{
		"csrf_token": {csrf},
		"name":       {"first"},
		"body":       {"v1"},
	}).Body.Close()

	var id int64
	if err := pool.QueryRow(
		context.Background(),
		`SELECT id FROM user_saved_replies WHERE user_id = $1 AND name = $2`,
		userID, "first",
	).Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}

	csrf = cli.extractCSRF(t, "/settings/saved-replies")
	resp := cli.post(t, "/settings/saved-replies/"+strconv.FormatInt(id, 10)+"/update", url.Values{
		"csrf_token": {csrf},
		"name":       {"first-renamed"},
		"body":       {"v2"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "updated") {
		t.Fatalf("update should succeed: %s", body)
	}

	var got string
	if err := pool.QueryRow(
		context.Background(),
		`SELECT body FROM user_saved_replies WHERE id = $1`, id,
	).Scan(&got); err != nil {
		t.Fatalf("read after update: %v", err)
	}
	if got != "v2" {
		t.Errorf("body: got %q, want v2", got)
	}

	csrf = cli.extractCSRF(t, "/settings/saved-replies")
	resp = cli.post(t, "/settings/saved-replies/"+strconv.FormatInt(id, 10)+"/delete", url.Values{
		"csrf_token": {csrf},
	})
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "deleted") {
		t.Fatalf("delete should succeed: %s", body)
	}
	assertSavedRepliesCount(t, pool, userID, 0)
}

// TestSavedReplies_DuplicateNameRejected exercises the
// (user_id, name) UNIQUE constraint via the friendly handler-side message.
func TestSavedReplies_DuplicateNameRejected(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, _ := newBillingTestUser(t, srv, pool, captor, "freedup")

	csrf := cli.extractCSRF(t, "/settings/saved-replies")
	cli.post(t, "/settings/saved-replies", url.Values{
		"csrf_token": {csrf},
		"name":       {"lgtm"},
		"body":       {"looks good"},
	}).Body.Close()

	csrf = cli.extractCSRF(t, "/settings/saved-replies")
	resp := cli.post(t, "/settings/saved-replies", url.Values{
		"csrf_token": {csrf},
		"name":       {"LGTM"},
		"body":       {"also fine, but same citext"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "already have a saved reply") {
		t.Fatalf("duplicate name should be rejected case-insensitively: %s", body)
	}
}

func assertSavedRepliesCount(t *testing.T, pool *pgxpool.Pool, userID int64, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM user_saved_replies WHERE user_id = $1`, userID,
	).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != want {
		t.Errorf("user_saved_replies count for %d: got %d, want %d", userID, got, want)
	}
}
