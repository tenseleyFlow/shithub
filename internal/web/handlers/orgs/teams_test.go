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
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	orgsh "github.com/tenseleyFlow/shithub/internal/web/handlers/orgs"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestTeamsListRequiresOrgMemberAndFiltersSecretTeams(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	memberID := insertOrgAvatarUser(t, pool, "member")
	outsiderID := insertOrgAvatarUser(t, pool, "outsider")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := pool.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, orgID, memberID); err != nil {
		t.Fatalf("insert org member: %v", err)
	}
	visibleTeamID := insertTeamForTest(t, pool, orgID, "engineering", "Engineering", "visible")
	insertTeamForTest(t, pool, orgID, "security", "Security", "secret")
	if _, err := pool.Exec(ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, visibleTeamID, memberID); err != nil {
		t.Fatalf("insert team member: %v", err)
	}

	memberBody, memberStatus, _ := performTeamsListRequest(t, pool, middleware.CurrentUser{ID: memberID, Username: "member"}, "/acme/teams")
	if memberStatus != http.StatusOK {
		t.Fatalf("member status=%d body=%s", memberStatus, memberBody)
	}
	if !strings.Contains(memberBody, "TEAM=engineering:Engineering:1:0") {
		t.Fatalf("expected visible team with counts, got %s", memberBody)
	}
	if strings.Contains(memberBody, "security") {
		t.Fatalf("secret team leaked to non-team member: %s", memberBody)
	}

	outsiderBody, outsiderStatus, _ := performTeamsListRequest(t, pool, middleware.CurrentUser{ID: outsiderID, Username: "outsider"}, "/acme/teams")
	if outsiderStatus != http.StatusNotFound {
		t.Fatalf("outsider status=%d body=%s", outsiderStatus, outsiderBody)
	}

	_, anonymousStatus, anonymousLocation := performTeamsListRequest(t, pool, middleware.CurrentUser{}, "/acme/teams")
	if anonymousStatus != http.StatusSeeOther {
		t.Fatalf("anonymous status=%d", anonymousStatus)
	}
	if !strings.HasPrefix(anonymousLocation, "/login?next=") {
		t.Fatalf("anonymous redirect=%q", anonymousLocation)
	}
}

func TestTeamMemberAddRejectsNonOrgUsers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	outsiderID := insertOrgAvatarUser(t, pool, "outsider")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	teamID := insertTeamForTest(t, pool, orgID, "engineering", "Engineering", "visible")

	form := url.Values{"user_id": {strconv.FormatInt(outsiderID, 10)}, "role": {"member"}}
	body, status, _ := performTeamsRequest(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, http.MethodPost, "/acme/teams/engineering/members", form)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_members WHERE team_id = $1`, teamID).Scan(&count); err != nil {
		t.Fatalf("count team members: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no team member insert, got %d", count)
	}
}

func TestTeamCreateBlocksSecretTeamsWithoutEntitlement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")

	form := url.Values{
		"display_name": {"Security"},
		"slug":         {"security"},
		"privacy":      {"secret"},
	}
	body, status, location := performTeamsRequest(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, http.MethodPost, "/acme/teams", form)
	if status != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if location != "/acme/teams?notice=secret-teams-upgrade" {
		t.Fatalf("redirect=%q", location)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM teams WHERE org_id = $1`, orgID).Scan(&count); err != nil {
		t.Fatalf("count teams: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no secret team insert, got %d", count)
	}
}

func TestSecretTeamAddMemberRequiresEntitlementButRemoveAllowed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	memberID := insertOrgAvatarUser(t, pool, "member")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := pool.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, orgID, memberID); err != nil {
		t.Fatalf("insert org member: %v", err)
	}
	teamID := insertTeamForTest(t, pool, orgID, "security", "Security", "secret")

	form := url.Values{"user_id": {strconv.FormatInt(memberID, 10)}, "role": {"member"}}
	body, status, location := performTeamsRequest(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, http.MethodPost, "/acme/teams/security/members", form)
	if status != http.StatusSeeOther {
		t.Fatalf("free add status=%d body=%s", status, body)
	}
	if location != "/acme/teams/security?notice=secret-teams-upgrade" {
		t.Fatalf("free add redirect=%q", location)
	}
	assertTeamMemberCount(t, pool, teamID, 0)

	if _, err := pool.Exec(ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, teamID, memberID); err != nil {
		t.Fatalf("seed team member: %v", err)
	}
	assertTeamMemberCount(t, pool, teamID, 1)

	remove := url.Values{
		"user_id": {strconv.FormatInt(memberID, 10)},
		"action":  {"remove"},
	}
	body, status, _ = performTeamsRequest(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, http.MethodPost, "/acme/teams/security/members", remove)
	if status != http.StatusSeeOther {
		t.Fatalf("remove status=%d body=%s", status, body)
	}
	assertTeamMemberCount(t, pool, teamID, 0)

	activateTeamPlanForTest(t, pool, orgID)
	body, status, _ = performTeamsRequest(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, http.MethodPost, "/acme/teams/security/members", form)
	if status != http.StatusSeeOther {
		t.Fatalf("team add status=%d body=%s", status, body)
	}
	assertTeamMemberCount(t, pool, teamID, 1)
}

func TestSecretTeamRepoGrantRequiresEntitlementButRevokeAllowed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	teamID := insertTeamForTest(t, pool, orgID, "security", "Security", "secret")
	repoID := insertTeamRepoForTest(t, pool, orgID, "private-repo")

	form := url.Values{"repo_id": {strconv.FormatInt(repoID, 10)}, "role": {"write"}}
	body, status, location := performTeamsRequest(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, http.MethodPost, "/acme/teams/security/repos", form)
	if status != http.StatusSeeOther {
		t.Fatalf("free grant status=%d body=%s", status, body)
	}
	if location != "/acme/teams/security?notice=secret-teams-upgrade" {
		t.Fatalf("free grant redirect=%q", location)
	}
	assertTeamRepoGrantCount(t, pool, teamID, 0)

	if _, err := pool.Exec(ctx, `INSERT INTO team_repo_access (team_id, repo_id, role) VALUES ($1, $2, 'write')`, teamID, repoID); err != nil {
		t.Fatalf("seed team repo access: %v", err)
	}
	assertTeamRepoGrantCount(t, pool, teamID, 1)

	remove := url.Values{
		"repo_id": {strconv.FormatInt(repoID, 10)},
		"action":  {"remove"},
	}
	body, status, _ = performTeamsRequest(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, http.MethodPost, "/acme/teams/security/repos", remove)
	if status != http.StatusSeeOther {
		t.Fatalf("revoke status=%d body=%s", status, body)
	}
	assertTeamRepoGrantCount(t, pool, teamID, 0)

	activateTeamPlanForTest(t, pool, orgID)
	body, status, _ = performTeamsRequest(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, http.MethodPost, "/acme/teams/security/repos", form)
	if status != http.StatusSeeOther {
		t.Fatalf("team grant status=%d body=%s", status, body)
	}
	assertTeamRepoGrantCount(t, pool, teamID, 1)
}

func performTeamsListRequest(t *testing.T, pool *pgxpool.Pool, viewer middleware.CurrentUser, target string) (string, int, string) {
	return performTeamsRequest(t, pool, viewer, http.MethodGet, target, nil)
}

func performTeamsRequest(t *testing.T, pool *pgxpool.Pool, viewer middleware.CurrentUser, method, target string, form url.Values) (string, int, string) {
	t.Helper()
	rr, err := render.New(fstest.MapFS{
		"_layout.html":         {Data: []byte(`{{ define "layout" }}<html><body>{{ template "page" . }}</body></html>{{ end }}`)},
		"orgs/teams_list.html": {Data: []byte(`{{ define "page" }}ACTIVE={{ .ActiveOrgNav }} TOTAL={{ .TeamTotalCount }}{{ range .Teams }} TEAM={{ .Slug }}:{{ .DisplayName }}:{{ .MemberCount }}:{{ .RepoCount }}{{ end }}{{ end }}`)},
		"orgs/team_view.html":  {Data: []byte(`{{ define "page" }}TEAM{{ end }}`)},
		"orgs/people.html":     {Data: []byte(`{{ define "page" }}PEOPLE{{ end }}`)},
		"errors/400.html":      {Data: []byte(`{{ define "page" }}400{{ end }}`)},
		"errors/403.html":      {Data: []byte(`{{ define "page" }}403{{ end }}`)},
		"errors/404.html":      {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":      {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	h, err := orgsh.New(orgsh.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render: rr,
		Pool:   pool,
	})
	if err != nil {
		t.Fatalf("orgsh.New: %v", err)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountOrgRoutes(r)

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Body.String(), rec.Code, rec.Header().Get("Location")
}

func insertTeamForTest(t *testing.T, db orgsdb.DBTX, orgID int64, slug, displayName, privacy string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(context.Background(),
		`INSERT INTO teams (org_id, slug, display_name, privacy)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		orgID, slug, displayName, privacy,
	).Scan(&id); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	return id
}

func insertTeamRepoForTest(t *testing.T, db orgsdb.DBTX, orgID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(context.Background(),
		`INSERT INTO repos (owner_org_id, name, visibility, default_branch)
		 VALUES ($1, $2, 'private', 'trunk')
		 RETURNING id`,
		orgID, name,
	).Scan(&id); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return id
}

func activateTeamPlanForTest(t *testing.T, pool *pgxpool.Pool, orgID int64) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	_, err := billing.ApplySubscriptionSnapshot(context.Background(), billing.Deps{Pool: pool}, billing.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_teams_" + strconv.FormatInt(orgID, 10),
		StripeSubscriptionItemID: "si_teams_" + strconv.FormatInt(orgID, 10),
		CurrentPeriodStart:       now,
		CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_teams_" + strconv.FormatInt(orgID, 10),
	})
	if err != nil {
		t.Fatalf("activate team plan: %v", err)
	}
}

func assertTeamMemberCount(t *testing.T, db orgsdb.DBTX, teamID int64, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM team_members WHERE team_id = $1`, teamID).Scan(&count); err != nil {
		t.Fatalf("count team members: %v", err)
	}
	if count != want {
		t.Fatalf("team member count=%d, want %d", count, want)
	}
}

func assertTeamRepoGrantCount(t *testing.T, db orgsdb.DBTX, teamID int64, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM team_repo_access WHERE team_id = $1`, teamID).Scan(&count); err != nil {
		t.Fatalf("count team repo grants: %v", err)
	}
	if count != want {
		t.Fatalf("team repo grant count=%d, want %d", count, want)
	}
}
