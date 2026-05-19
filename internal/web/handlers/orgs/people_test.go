// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	"github.com/tenseleyFlow/shithub/internal/web"
	orgsh "github.com/tenseleyFlow/shithub/internal/web/handlers/orgs"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestOrgPeoplePageSearchAndOwnerChrome(t *testing.T) {
	t.Parallel()
	env := setupOrgPeopleEnv(t)

	ownerID := insertOrgPeopleUser(t, env.pool, "mfwolffe", "Matt Wolffe")
	orgID := insertOrgPeopleOrg(t, env.pool, ownerID, "tenseleyFlow")
	bobID := insertOrgPeopleUser(t, env.pool, "bob", "Bob Builder")
	carolID := insertOrgPeopleUser(t, env.pool, "carol", "Carol Coder")
	insertOrgPeopleMember(t, env.pool, orgID, bobID, "member")
	insertOrgPeopleMember(t, env.pool, orgID, carolID, "member")

	body := env.get(t, "/tenseleyFlow/people?query=BUILDER", ownerID, "mfwolffe")
	for _, want := range []string{
		`class="shithub-org-pagehead"`,
		`Organization permissions`,
		`Find a member...`,
		`Invite member`,
		`Bob Builder`,
		`value="BUILDER"`,
		`shithub-tab-count">3`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("owner search body missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "Carol Coder") {
		t.Fatalf("filtered search should not include Carol Coder\n%s", body)
	}

	body = env.get(t, "/tenseleyFlow/people", 0, "")
	for _, want := range []string{`Bob Builder`, `Carol Coder`, `Matt Wolffe`} {
		if !strings.Contains(body, want) {
			t.Fatalf("anonymous member list missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "Invite member") {
		t.Fatalf("anonymous people page should not show owner-only invite control\n%s", body)
	}

	body = env.get(t, "/tenseleyFlow/people?query=missing", ownerID, "mfwolffe")
	if !strings.Contains(body, "No members matched your search.") {
		t.Fatalf("expected search empty state\n%s", body)
	}
}

func TestOrgInvitationInboxAcceptsUsernameInvite(t *testing.T) {
	t.Parallel()
	env := setupOrgPeopleEnv(t)

	ownerID := insertOrgPeopleUser(t, env.pool, "mfwolffe", "Matt Wolffe")
	orgID := insertOrgPeopleOrg(t, env.pool, ownerID, "tenseleyFlow")
	inviteeID := insertOrgPeopleUser(t, env.pool, "espadonne", "mfw")

	resp := env.postForm(t, "/tenseleyFlow/people/invite", ownerID, "mfwolffe", url.Values{
		"target": {"@espadonne"},
		"role":   {"member"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("invite status=%d", resp.StatusCode)
	}

	var invitationID int64
	var targetUserID int64
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id, target_user_id FROM org_invitations WHERE org_id=$1`,
		orgID,
	).Scan(&invitationID, &targetUserID); err != nil {
		t.Fatalf("lookup invitation: %v", err)
	}
	if targetUserID != inviteeID {
		t.Fatalf("target user id: got %d want %d", targetUserID, inviteeID)
	}

	body := env.get(t, "/invitations", inviteeID, "espadonne")
	for _, want := range []string{
		"Organization invitations",
		"tenseleyFlow",
		"/invitations/id/" + strconv.FormatInt(invitationID, 10) + "/accept",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("invitations inbox missing %q\n%s", want, body)
		}
	}

	body = env.get(t, "/tenseleyFlow/people", inviteeID, "espadonne")
	if !strings.Contains(body, "You've been invited to join tenseleyFlow.") {
		t.Fatalf("people page missing invite callout\n%s", body)
	}

	resp = env.postForm(t, "/invitations/id/"+strconv.FormatInt(invitationID, 10)+"/accept", inviteeID, "espadonne", url.Values{})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/tenseleyFlow" {
		t.Fatalf("accept status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var count int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM org_members WHERE org_id=$1 AND user_id=$2 AND role='member'`,
		orgID, inviteeID,
	).Scan(&count); err != nil {
		t.Fatalf("lookup membership: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected accepted invite to add membership, got %d rows", count)
	}
}

type orgPeopleEnv struct {
	srv  *httptest.Server
	pool *pgxpool.Pool
}

func setupOrgPeopleEnv(t *testing.T) *orgPeopleEnv {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	rr, err := render.New(web.TemplatesFS(), render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	h, err := orgsh.New(orgsh.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render: rr,
		Pool:   pool,
	})
	if err != nil {
		t.Fatalf("orgs.New: %v", err)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawID := r.Header.Get("X-Test-User-ID")
			if rawID == "" {
				next.ServeHTTP(w, r)
				return
			}
			id, err := strconv.ParseInt(rawID, 10, 64)
			if err != nil {
				t.Fatalf("bad X-Test-User-ID: %v", err)
			}
			viewer := middleware.CurrentUser{ID: id, Username: r.Header.Get("X-Test-Username")}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountOrgRoutes(r)
	h.MountInvitations(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &orgPeopleEnv{srv: srv, pool: pool}
}

func (e *orgPeopleEnv) get(t *testing.T, path string, userID int64, username string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if userID != 0 {
		req.Header.Set("X-Test-User-ID", strconv.FormatInt(userID, 10))
		req.Header.Set("X-Test-Username", username)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, resp.StatusCode, body)
	}
	return string(body)
}

func (e *orgPeopleEnv) postForm(t *testing.T, path string, userID int64, username string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if userID != 0 {
		req.Header.Set("X-Test-User-ID", strconv.FormatInt(userID, 10))
		req.Header.Set("X-Test-Username", username)
	}
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

const orgPeopleFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func insertOrgPeopleUser(t *testing.T, db *pgxpool.Pool, username, display string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(context.Background(),
		`INSERT INTO users (username, display_name, password_hash)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		username, display, orgPeopleFixtureHash,
	).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	return id
}

func insertOrgPeopleOrg(t *testing.T, db *pgxpool.Pool, ownerID int64, slug string) int64 {
	t.Helper()
	var orgID int64
	if err := db.QueryRow(context.Background(),
		`INSERT INTO orgs (slug, display_name, created_by_user_id)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		slug, slug, ownerID,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	insertOrgPeopleMember(t, db, orgID, ownerID, "owner")
	return orgID
}

func insertOrgPeopleMember(t *testing.T, db *pgxpool.Pool, orgID, userID int64, role string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO org_members (org_id, user_id, role)
		 VALUES ($1, $2, $3)`,
		orgID, userID, role,
	); err != nil {
		t.Fatalf("insert org member: %v", err)
	}
}
