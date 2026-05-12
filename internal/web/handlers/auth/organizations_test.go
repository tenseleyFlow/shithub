// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/orgs"
)

func TestSettingsOrganizationsListsMemberships(t *testing.T) {
	t.Parallel()
	httpsrv, pool, captor := newTestServerWithPool(t, false)
	cli := newClient(t, httpsrv)

	createLoggedInOwnerOrg(t, cli, pool, captor, "aliceorg", "alice-org")

	resp := cli.get(t, "/settings/organizations")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get organizations: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	for _, want := range []string{
		"Organizations",
		"USER=aliceorg",
		"alice-org:Owner:manage=true",
		"compare=;",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q in body: %s", want, body)
		}
	}
}

func TestSettingsOrganizationsLinksComparePlansWhenBillingEnabled(t *testing.T) {
	t.Parallel()
	httpsrv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		RequireVerify:     false,
		OrgBillingEnabled: true,
	})
	cli := newClient(t, httpsrv)

	createLoggedInOwnerOrg(t, cli, pool, captor, "billingorg", "billing-org")

	resp := cli.get(t, "/settings/organizations")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get organizations: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	want := "billing-org:Owner:manage=true:compare=/organizations/billing-org/settings/billing#compare-plans;"
	if !strings.Contains(string(body), want) {
		t.Fatalf("missing billing compare href %q in body: %s", want, body)
	}
}

func createLoggedInOwnerOrg(t *testing.T, cli *client, pool *pgxpool.Pool, captor *captureSender, username, orgSlug string) {
	t.Helper()
	email := username + "@example.com"
	mustSignup(t, cli, username, email, "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {username},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	var userID int64
	if err := pool.QueryRow(context.Background(), "SELECT id FROM users WHERE username = $1", username).Scan(&userID); err != nil {
		t.Fatalf("lookup user id: %v", err)
	}
	if _, err := orgs.Create(context.Background(), orgs.Deps{Pool: pool}, orgs.CreateParams{
		Slug:            orgSlug,
		DisplayName:     orgSlug,
		BillingEmail:    email,
		CreatedByUserID: userID,
	}); err != nil {
		t.Fatalf("create org: %v", err)
	}
}
