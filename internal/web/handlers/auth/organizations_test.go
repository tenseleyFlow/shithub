// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/orgs"
)

func TestSettingsOrganizationsListsMemberships(t *testing.T) {
	t.Parallel()
	httpsrv, pool, captor := newTestServerWithPool(t, false)
	cli := newClient(t, httpsrv)

	mustSignup(t, cli, "aliceorg", "aliceorg@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"aliceorg"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	var userID int64
	if err := pool.QueryRow(context.Background(), "SELECT id FROM users WHERE username = $1", "aliceorg").Scan(&userID); err != nil {
		t.Fatalf("lookup user id: %v", err)
	}
	if _, err := orgs.Create(context.Background(), orgs.Deps{Pool: pool}, orgs.CreateParams{
		Slug:            "alice-org",
		DisplayName:     "Alice Org",
		BillingEmail:    "aliceorg@example.com",
		CreatedByUserID: userID,
	}); err != nil {
		t.Fatalf("create org: %v", err)
	}

	resp = cli.get(t, "/settings/organizations")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get organizations: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	for _, want := range []string{
		"Organizations",
		"USER=aliceorg",
		"alice-org:Owner:manage=true",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q in body: %s", want, body)
		}
	}
}
